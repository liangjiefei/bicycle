package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gobicycle/bicycle/config"
	"github.com/gobicycle/bicycle/core"
	"github.com/gofrs/uuid"
	"github.com/shopspring/decimal"
	log "github.com/sirupsen/logrus"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"net/http"
	"strconv"
	"sync"
)

type Handler struct {
	storage          storage
	blockchain       blockchain
	token            string
	shard            byte
	mutex            sync.Mutex
	hotWalletAddress address.Address
}

type WithdrawalRequest struct {
	UserID      string     `json:"user_id"`
	QueryID     string     `json:"query_id"`
	Currency    string     `json:"currency"`
	Amount      core.Coins `json:"amount"`
	Destination string     `json:"destination"`
	Comment     string     `json:"comment"`
}

type ServiceTonWithdrawalRequest struct {
	From string `json:"from"`
}

type ServiceJettonWithdrawalRequest struct {
	Owner        string `json:"owner"`
	JettonMaster string `json:"jetton_master"`
}

type WalletAddress struct {
	Address  string `json:"address"`
	Currency string `json:"currency"`
}

type GetAddressesResponse struct {
	Addresses []WalletAddress `json:"addresses"`
}

type WithdrawalResponse struct {
	ID int64 `json:"ID"`
}

type WithdrawalStatusResponse struct {
	UserID  string                `json:"user_id"`
	QueryID string                `json:"query_id"`
	Status  core.WithdrawalStatus `json:"status"`
}

type GetIncomeResponse struct {
	Side         string        `json:"counting_side"`
	TotalIncomes []totalIncome `json:"total_income"`
}

type GetHistoryResponse struct {
	Incomes []income `json:"incomes"`
}

type totalIncome struct {
	Address  string `json:"deposit_address"`
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type income struct {
	DepositAddress string `json:"deposit_address"`
	Time           int64  `json:"time"`
	SourceAddress  string `json:"source_address,omitempty"`
	Amount         string `json:"amount"`
	Comment        string `json:"comment,omitempty"`
}

func NewHandler(s storage, b blockchain, token string, shard byte, hotWalletAddress address.Address) *Handler {
	return &Handler{storage: s, blockchain: b, token: token, shard: shard, hotWalletAddress: hotWalletAddress}
}

func (h *Handler) getNewAddress(resp http.ResponseWriter, req *http.Request) {
	var data struct {
		UserID   string `json:"user_id"`
		Currency string `json:"currency"`
	}
	err := json.NewDecoder(req.Body).Decode(&data)
	if err != nil {
		writeHttpError(resp, http.StatusBadRequest, fmt.Sprintf("decode payload data err: %v", err))
		return
	}
	if !isValidCurrency(data.Currency) {
		writeHttpError(resp, http.StatusBadRequest, "invalid currency type")
		return
	}
	h.mutex.Lock()
	defer h.mutex.Unlock() // To prevent data race
	addr, err := generateAddress(req.Context(), data.UserID, data.Currency, h.shard, h.storage, h.blockchain, h.hotWalletAddress)
	if err != nil {
		writeHttpError(resp, http.StatusInternalServerError, fmt.Sprintf("generate address err: %v", err))
		return
	}
	res := struct {
		Address string `json:"address"`
	}{Address: addr}
	resp.Header().Add("Content-Type", "application/json")
	resp.WriteHeader(http.StatusOK)
	err = json.NewEncoder(resp).Encode(res)
	if err != nil {
		log.Errorf("json encode error: %v", err)
	}
}

func (h *Handler) getAddresses(resp http.ResponseWriter, req *http.Request) {
	userID := req.URL.Query().Get("user_id")
	if userID == "" {
		writeHttpError(resp, http.StatusBadRequest, "need to provide user ID")
		return
	}
	addresses, err := getAddresses(req.Context(), userID, h.storage)
	if err != nil {
		writeHttpError(resp, http.StatusInternalServerError, fmt.Sprintf("get addresses err: %v", err))
		return
	}
	resp.Header().Add("Content-Type", "application/json")
	resp.WriteHeader(http.StatusOK)
	err = json.NewEncoder(resp).Encode(addresses)
	if err != nil {
		log.Errorf("json encode error: %v", err)
	}
}

func (h *Handler) sendWithdrawal(resp http.ResponseWriter, req *http.Request) {
	var body WithdrawalRequest
	err := json.NewDecoder(req.Body).Decode(&body)
	if err != nil {
		writeHttpError(resp, http.StatusBadRequest, fmt.Sprintf("decode payload err: %v", err))
		return
	}
	w, err := convertWithdrawal(body)
	if err != nil {
		writeHttpError(resp, http.StatusBadRequest, fmt.Sprintf("convert withdrawal err: %v", err))
		return
	}
	unique, err := h.storage.IsWithdrawalRequestUnique(req.Context(), w)
	if err != nil {
		writeHttpError(resp, http.StatusInternalServerError, fmt.Sprintf("check withdrawal uniquess err: %v", err))
		return
	} else if !unique {
		writeHttpError(resp, http.StatusBadRequest, "(user_id,query_id) not unique")
		return
	}
	_, ok := h.storage.GetWalletType(w.Destination)
	if ok {
		writeHttpError(resp, http.StatusBadRequest, "withdrawal to service internal addresses not supported")
		return
	}
	id, err := h.storage.SaveWithdrawalRequest(req.Context(), w)
	if err != nil {
		writeHttpError(resp, http.StatusInternalServerError, fmt.Sprintf("save withdrawal request err: %v", err))
		return
	}
	r := WithdrawalResponse{ID: id}
	resp.Header().Add("Content-Type", "application/json")
	resp.WriteHeader(http.StatusOK)
	err = json.NewEncoder(resp).Encode(r)
	if err != nil {
		log.Errorf("json encode error: %v", err)
	}
}

func (h *Handler) getSync(resp http.ResponseWriter, req *http.Request) {
	isSynced, err := h.storage.IsActualBlockData(req.Context())
	if err != nil {
		writeHttpError(resp, http.StatusInternalServerError, fmt.Sprintf("get sync from db err: %v", err))
		return
	}
	getSyncResponse := struct {
		IsSynced bool `json:"is_synced"`
	}{
		IsSynced: isSynced,
	}
	resp.Header().Add("Content-Type", "application/json")
	resp.WriteHeader(http.StatusOK)
	err = json.NewEncoder(resp).Encode(getSyncResponse)
	if err != nil {
		log.Errorf("json encode error: %v", err)
	}
}

func (h *Handler) getWithdrawalStatus(resp http.ResponseWriter, req *http.Request) {
	ids := req.URL.Query().Get("id")
	if ids == "" {
		writeHttpError(resp, http.StatusBadRequest, "need to provide request ID")
		return
	}
	id, err := strconv.ParseInt(ids, 10, 64)
	if err != nil {
		writeHttpError(resp, http.StatusBadRequest, fmt.Sprintf("convert request ID err: %v", err))
		return
	}
	status, err := h.storage.GetExternalWithdrawalStatus(req.Context(), id)
	if errors.Is(err, core.ErrNotFound) {
		writeHttpError(resp, http.StatusBadRequest, "request ID not found")
		return
	}
	if err != nil {
		writeHttpError(resp, http.StatusInternalServerError, fmt.Sprintf("get external withdrawal status err: %v", err))
		return
	}
	resp.Header().Add("Content-Type", "application/json")
	resp.WriteHeader(http.StatusOK)
	err = json.NewEncoder(resp).Encode(WithdrawalStatusResponse{
		UserID:  status.UserID,
		QueryID: status.QueryID,
		Status:  status.Status,
	})
	if err != nil {
		log.Errorf("json encode error: %v", err)
	}
}

func (h *Handler) getIncome(resp http.ResponseWriter, req *http.Request) {
	id := req.URL.Query().Get("user_id")
	if id == "" {
		writeHttpError(resp, http.StatusBadRequest, "need to provide user ID")
		return
	}
	totalIncomes, err := h.storage.GetIncome(req.Context(), id, config.Config.IsDepositSideCalculation)
	if err != nil {
		writeHttpError(resp, http.StatusInternalServerError, fmt.Sprintf("get balances err: %v", err))
		return
	}
	resp.Header().Add("Content-Type", "application/json")
	resp.WriteHeader(http.StatusOK)
	err = json.NewEncoder(resp).Encode(convertIncome(h.storage, totalIncomes))
	if err != nil {
		log.Errorf("json encode error: %v", err)
	}
}

func (h *Handler) getIncomeHistory(resp http.ResponseWriter, req *http.Request) {
	id := req.URL.Query().Get("user_id")
	if id == "" {
		writeHttpError(resp, http.StatusBadRequest, "need to provide user ID")
		return
	}
	currency := req.URL.Query().Get("currency")
	if currency == "" {
		writeHttpError(resp, http.StatusBadRequest, "need to provide currency")
		return
	}
	if !isValidCurrency(currency) {
		writeHttpError(resp, http.StatusBadRequest, "invalid currency type")
		return
	}
	limit, err := strconv.Atoi(req.URL.Query().Get("limit"))
	if err != nil {
		writeHttpError(resp, http.StatusBadRequest, "invalid limit parameter")
		return
	}
	offset, err := strconv.Atoi(req.URL.Query().Get("offset"))
	if err != nil {
		writeHttpError(resp, http.StatusBadRequest, "invalid offset parameter")
		return
	}
	history, err := h.storage.GetIncomeHistory(req.Context(), id, currency, limit, offset)
	if err != nil {
		writeHttpError(resp, http.StatusInternalServerError, fmt.Sprintf("get history err: %v", err))
		return
	}
	resp.Header().Add("Content-Type", "application/json")
	resp.WriteHeader(http.StatusOK)
	err = json.NewEncoder(resp).Encode(convertHistory(h.storage, currency, history))
	if err != nil {
		log.Errorf("json encode error: %v", err)
	}
}

func (h *Handler) serviceTonWithdrawal(resp http.ResponseWriter, req *http.Request) {
	var body ServiceTonWithdrawalRequest
	err := json.NewDecoder(req.Body).Decode(&body)
	if err != nil {
		writeHttpError(resp, http.StatusBadRequest, fmt.Sprintf("decode payload err: %v", err))
		return
	}
	w, err := convertTonServiceWithdrawal(h.storage, body)
	if err != nil {
		writeHttpError(resp, http.StatusBadRequest, fmt.Sprintf("convert service withdrawal err: %v", err))
		return
	}
	memo, err := h.storage.SaveServiceWithdrawalRequest(req.Context(), w)
	if err != nil {
		writeHttpError(resp, http.StatusInternalServerError, fmt.Sprintf("save service withdrawal request err: %v", err))
		return
	}
	var response = struct {
		Memo uuid.UUID `json:"memo"`
	}{
		Memo: memo,
	}
	resp.Header().Add("Content-Type", "application/json")
	resp.WriteHeader(http.StatusOK)
	err = json.NewEncoder(resp).Encode(response)
	if err != nil {
		log.Errorf("json encode error: %v", err)
	}
}

func (h *Handler) serviceJettonWithdrawal(resp http.ResponseWriter, req *http.Request) {
	var body ServiceJettonWithdrawalRequest
	err := json.NewDecoder(req.Body).Decode(&body)
	if err != nil {
		writeHttpError(resp, http.StatusBadRequest, fmt.Sprintf("decode payload err: %v", err))
		return
	}
	w, err := convertJettonServiceWithdrawal(h.storage, body)
	if err != nil {
		writeHttpError(resp, http.StatusBadRequest, fmt.Sprintf("convert service withdrawal err: %v", err))
		return
	}
	memo, err := h.storage.SaveServiceWithdrawalRequest(req.Context(), w)
	if err != nil {
		writeHttpError(resp, http.StatusInternalServerError, fmt.Sprintf("save service withdrawal request err: %v", err))
		return
	}
	var response = struct {
		Memo uuid.UUID `json:"memo"`
	}{
		Memo: memo,
	}
	resp.Header().Add("Content-Type", "application/json")
	resp.WriteHeader(http.StatusOK)
	err = json.NewEncoder(resp).Encode(response)
	if err != nil {
		log.Errorf("json encode error: %v", err)
	}
}

func RegisterHandlers(mux *http.ServeMux, h *Handler) {
	mux.HandleFunc("/v1/address/new", recoverMiddleware(authMiddleware(post(h.getNewAddress))))
	mux.HandleFunc("/v1/address/all", recoverMiddleware(authMiddleware(get(h.getAddresses))))
	mux.HandleFunc("/v1/withdrawal/send", recoverMiddleware(authMiddleware(post(h.sendWithdrawal))))
	mux.HandleFunc("/v1/withdrawal/service/ton", recoverMiddleware(authMiddleware(post(h.serviceTonWithdrawal))))
	mux.HandleFunc("/v1/withdrawal/service/jetton", recoverMiddleware(authMiddleware(post(h.serviceJettonWithdrawal))))
	mux.HandleFunc("/v1/withdrawal/status", recoverMiddleware(authMiddleware(get(h.getWithdrawalStatus))))
	mux.HandleFunc("/v1/system/sync", recoverMiddleware(get(h.getSync)))
	mux.HandleFunc("/v1/balance", recoverMiddleware(authMiddleware(get(h.getBalance)))) // deprecated
	mux.HandleFunc("/v1/income", recoverMiddleware(authMiddleware(get(h.getIncome))))
	mux.HandleFunc("/v1/deposit/history", recoverMiddleware(authMiddleware(get(h.getIncomeHistory))))
}

func generateAddress(
	ctx context.Context,
	userID string,
	currency string,
	shard byte,
	dbConn storage,
	bc blockchain,
	hotWalletAddress address.Address,
) (
	string,
	error,
) {
	subwalletID, err := dbConn.GetLastSubwalletID(ctx)
	if err != nil {
		return "", err
	}
	var res string
	if currency == core.TonSymbol {
		w, id, err := bc.GenerateSubWallet(config.Config.Seed, shard, subwalletID+1)
		if err != nil {
			return "", err
		}
		a, err := core.AddressFromTonutilsAddress(w.Address())
		if err != nil {
			return "", err
		}
		err = dbConn.SaveTonWallet(ctx,
			core.WalletData{
				SubwalletID: id,
				UserID:      userID,
				Currency:    core.TonSymbol,
				Type:        core.TonDepositWallet,
				Address:     a,
			},
		)
		if err != nil {
			return "", err
		}
		res = a.ToUserFormat()
	} else {
		jetton, ok := config.Config.Jettons[currency]
		if !ok {
			return "", fmt.Errorf("jetton address not found")
		}
		proxy, addr, err := bc.GenerateDepositJettonWalletForProxy(ctx, shard, &hotWalletAddress, jetton.Master, subwalletID+1)
		if err != nil {
			return "", err
		}
		jettonWalletAddr, err := core.AddressFromTonutilsAddress(addr)
		if err != nil {
			return "", err
		}
		proxyAddr, err := core.AddressFromTonutilsAddress(proxy.Address())
		if err != nil {
			return "", err
		}
		err = dbConn.SaveJettonWallet(
			ctx,
			proxyAddr,
			core.WalletData{
				UserID:      userID,
				SubwalletID: proxy.SubwalletID,
				Currency:    currency,
				Type:        core.JettonDepositWallet,
				Address:     jettonWalletAddr,
			},
			false,
		)
		if err != nil {
			return "", err
		}
		res = proxyAddr.ToUserFormat()
	}
	return res, nil
}

func getAddresses(ctx context.Context, userID string, dbConn storage) (GetAddressesResponse, error) {
	var res = GetAddressesResponse{
		Addresses: []WalletAddress{},
	}
	tonAddr, err := dbConn.GetTonWalletsAddresses(ctx, userID, []core.WalletType{core.TonDepositWallet})
	if err != nil {
		return GetAddressesResponse{}, err
	}
	jettonAddr, err := dbConn.GetJettonOwnersAddresses(ctx, userID, []core.WalletType{core.JettonDepositWallet})
	if err != nil {
		return GetAddressesResponse{}, err
	}
	for _, a := range tonAddr {
		res.Addresses = append(res.Addresses, WalletAddress{Address: a.ToUserFormat(), Currency: core.TonSymbol})
	}
	for _, a := range jettonAddr {
		res.Addresses = append(res.Addresses, WalletAddress{Address: a.Address.ToUserFormat(), Currency: a.Currency})
	}
	return res, nil
}

func isValidCurrency(cur string) bool {
	if _, ok := config.Config.Jettons[cur]; ok || cur == core.TonSymbol {
		return true
	}
	return false
}

func convertWithdrawal(w WithdrawalRequest) (core.WithdrawalRequest, error) {
	if !isValidCurrency(w.Currency) {
		return core.WithdrawalRequest{}, fmt.Errorf("invalid currency")
	}
	addr, bounceable, err := validateAddress(w.Destination)
	if err != nil {
		return core.WithdrawalRequest{}, fmt.Errorf("invalid destination address: %v", err)
	}
	if !(w.Amount.Cmp(decimal.New(0, 0)) == 1) {
		return core.WithdrawalRequest{}, fmt.Errorf("amount must be > 0")
	}
	return core.WithdrawalRequest{
		UserID:      w.UserID,
		QueryID:     w.QueryID,
		Currency:    w.Currency,
		Amount:      w.Amount,
		Destination: addr,
		Bounceable:  bounceable,
		Comment:     w.Comment,
		IsInternal:  false,
	}, nil
}

func convertTonServiceWithdrawal(s storage, w ServiceTonWithdrawalRequest) (core.ServiceWithdrawalRequest, error) {
	from, _, err := validateAddress(w.From)
	if err != nil {
		return core.ServiceWithdrawalRequest{}, fmt.Errorf("invalid from address: %v", err)
	}
	t, ok := s.GetWalletType(from)
	if !ok {
		return core.ServiceWithdrawalRequest{}, fmt.Errorf("unknown deposit address")
	}
	if t != core.JettonOwner {
		return core.ServiceWithdrawalRequest{},
			fmt.Errorf("service withdrawal allowed only for Jetton deposit owner")
	}
	return core.ServiceWithdrawalRequest{
		From: from,
	}, nil
}

func convertJettonServiceWithdrawal(s storage, w ServiceJettonWithdrawalRequest) (core.ServiceWithdrawalRequest, error) {
	from, _, err := validateAddress(w.Owner)
	if err != nil {
		return core.ServiceWithdrawalRequest{}, fmt.Errorf("invalid from address: %v", err)
	}
	t, ok := s.GetWalletType(from)
	if !ok {
		return core.ServiceWithdrawalRequest{}, fmt.Errorf("unknown deposit address")
	}
	if t != core.JettonOwner && t != core.TonDepositWallet {
		return core.ServiceWithdrawalRequest{},
			fmt.Errorf("service withdrawal allowed only for Jetton deposit owner or TON deposit")
	}
	jetton, _, err := validateAddress(w.JettonMaster)
	if err != nil {
		return core.ServiceWithdrawalRequest{}, fmt.Errorf("invalid jetton master address: %v", err)
	}
	// currency type checks by withdrawal processor
	return core.ServiceWithdrawalRequest{
		From:         from,
		JettonMaster: &jetton,
	}, nil
}

func convertIncome(dbConn storage, totalIncomes []core.TotalIncome) GetIncomeResponse {
	var res = GetIncomeResponse{
		TotalIncomes: []totalIncome{},
	}
	if config.Config.IsDepositSideCalculation {
		res.Side = core.SideDeposit
	} else {
		res.Side = core.SideHotWallet
	}

	for _, b := range totalIncomes {
		totIncome := totalIncome{
			Amount:   b.Amount.String(),
			Currency: b.Currency,
		}
		if b.Currency == core.TonSymbol {
			totIncome.Address = b.Deposit.ToUserFormat()
		} else {
			owner := dbConn.GetOwner(b.Deposit)
			if owner == nil {
				// TODO: remove fatal
				log.Fatalf("can not find owner for deposit: %s", b.Deposit.ToUserFormat())
			}
			totIncome.Address = owner.ToUserFormat()
		}
		res.TotalIncomes = append(res.TotalIncomes, totIncome)
	}
	return res
}

func convertHistory(dbConn storage, currency string, incomes []core.ExternalIncome) GetHistoryResponse {
	var res = GetHistoryResponse{
		Incomes: []income{},
	}
	for _, i := range incomes {
		inc := income{
			Time:    int64(i.Utime),
			Amount:  i.Amount.String(),
			Comment: i.Comment,
		}
		if currency == core.TonSymbol {
			inc.DepositAddress = i.To.ToUserFormat()
		} else {
			owner := dbConn.GetOwner(i.To)
			if owner == nil {
				// TODO: remove fatal
				log.Fatalf("can not find owner for deposit: %s", i.To.ToUserFormat())
			}
			inc.DepositAddress = owner.ToUserFormat()
		}
		// show only std address
		if len(i.From) == 32 && i.FromWorkchain != nil {
			addr := address.NewAddress(0, byte(*i.FromWorkchain), i.From)
			addr.SetTestnetOnly(config.Config.Testnet)
			inc.SourceAddress = addr.String()
		}
		res.Incomes = append(res.Incomes, inc)
	}
	return res
}

func validateAddress(addr string) (core.Address, bool, error) {
	if addr == "" {
		return core.Address{}, false, fmt.Errorf("empty address")
	}
	a, err := address.ParseAddr(addr)
	if err != nil {
		return core.Address{}, false, fmt.Errorf("invalid address: %v", err)
	}
	if a.IsTestnetOnly() && !config.Config.Testnet {
		return core.Address{}, false, fmt.Errorf("address for testnet only")
	}
	if a.Workchain() != core.DefaultWorkchain {
		return core.Address{}, false, fmt.Errorf("address must be in %d workchain",
			core.DefaultWorkchain)
	}
	res, err := core.AddressFromTonutilsAddress(a)
	return res, a.IsBounceable(), err
}

type storage interface {
	GetLastSubwalletID(ctx context.Context) (uint32, error)
	SaveTonWallet(ctx context.Context, walletData core.WalletData) error
	SaveJettonWallet(ctx context.Context, ownerAddress core.Address, walletData core.WalletData, notSaveOwner bool) error
	GetTonWalletsAddresses(ctx context.Context, userID string, types []core.WalletType) ([]core.Address, error)
	GetJettonOwnersAddresses(ctx context.Context, userID string, types []core.WalletType) ([]core.OwnerWallet, error)
	SaveWithdrawalRequest(ctx context.Context, w core.WithdrawalRequest) (int64, error)
	IsWithdrawalRequestUnique(ctx context.Context, w core.WithdrawalRequest) (bool, error)
	IsActualBlockData(ctx context.Context) (bool, error)
	GetExternalWithdrawalStatus(ctx context.Context, id int64) (core.WithdrawalData, error)
	GetWalletType(address core.Address) (core.WalletType, bool)
	GetIncome(ctx context.Context, userID string, isDepositSide bool) ([]core.TotalIncome, error)
	SaveServiceWithdrawalRequest(ctx context.Context, w core.ServiceWithdrawalRequest) (uuid.UUID, error)
	GetIncomeHistory(ctx context.Context, userID string, currency string, limit int, offset int) ([]core.ExternalIncome, error)
	GetOwner(address core.Address) *core.Address
}

type blockchain interface {
	GenerateSubWallet(seed string, shard byte, startSubWalletID uint32) (*wallet.Wallet, uint32, error)
	GenerateDepositJettonWalletForProxy(
		ctx context.Context,
		shard byte,
		proxyOwner, jettonMaster *address.Address,
		startSubWalletID uint32,
	) (
		proxy *core.JettonProxy,
		addr *address.Address,
		err error,
	)
	GenerateDefaultWallet(seed string, isHighload bool) (*wallet.Wallet, byte, uint32, error)
}
