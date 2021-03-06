package api

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"encoding/json"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/skycoin/skycoin/src/cipher"
	"github.com/skycoin/skycoin/src/cipher/bip39"
	"github.com/skycoin/skycoin/src/cipher/crypto"
	"github.com/skycoin/skycoin/src/coin"
	"github.com/skycoin/skycoin/src/readable"
	"github.com/skycoin/skycoin/src/testutil"
	"github.com/skycoin/skycoin/src/visor"
	"github.com/skycoin/skycoin/src/wallet"
	"github.com/skycoin/skycoin/src/wallet/deterministic"
)

func TestGetBalanceHandler(t *testing.T) {
	type httpBody struct {
		addrs string
	}
	invalidAddr := "invalidAddr"
	validAddr := "2eZYSbzBKJ7QCL4kd5LSqV478rJQGb4UNkf"
	address, err := cipher.DecodeBase58Address(validAddr)
	require.NoError(t, err)
	tt := []struct {
		name                      string
		method                    string
		status                    int
		err                       string
		httpBody                  *httpBody
		getBalanceOfAddrsArg      []cipher.Address
		getBalanceOfAddrsResponse []wallet.BalancePair
		getBalanceOfAddrsError    error
		httpResponse              readable.BalancePair
	}{
		{
			name:   "405",
			method: http.MethodDelete,
			status: http.StatusMethodNotAllowed,
			err:    "405 Method Not Allowed",
		},
		{
			name:   "400 - invalid address",
			method: http.MethodGet,
			status: http.StatusBadRequest,
			err:    "400 Bad Request - address \"invalidAddr\" is invalid: Invalid base58 character",
			httpBody: &httpBody{
				addrs: invalidAddr,
			},
		},
		{
			name:     "400 - no addresses",
			method:   http.MethodGet,
			status:   http.StatusBadRequest,
			err:      "400 Bad Request - addrs is required",
			httpBody: &httpBody{},
		},
		{
			name:   "500 - GetBalanceOfAddrsError",
			method: http.MethodGet,
			status: http.StatusInternalServerError,
			err:    "500 Internal Server Error - gateway.GetBalanceOfAddresses failed: GetBalanceOfAddrsError",
			httpBody: &httpBody{
				addrs: validAddr,
			},
			getBalanceOfAddrsArg:   []cipher.Address{address},
			getBalanceOfAddrsError: errors.New("GetBalanceOfAddrsError"),
		},
		{
			name:   "500 - balance Confirmed coins uint64 addition overflow",
			method: http.MethodGet,
			status: http.StatusInternalServerError,
			err:    "500 Internal Server Error - uint64 addition overflow",
			httpBody: &httpBody{
				addrs: validAddr,
			},
			getBalanceOfAddrsArg: []cipher.Address{address},
			getBalanceOfAddrsResponse: []wallet.BalancePair{
				{
					Confirmed: wallet.Balance{Coins: math.MaxInt64 + 1, Hours: 0},
					Predicted: wallet.Balance{Coins: 0, Hours: 0},
				},
				{
					Confirmed: wallet.Balance{Coins: math.MaxInt64 + 1, Hours: 0},
					Predicted: wallet.Balance{Coins: 0, Hours: 0},
				},
			},
		},
		{
			name:   "500 - balance Predicted coins uint64 addition overflow",
			method: http.MethodGet,
			status: http.StatusInternalServerError,
			err:    "500 Internal Server Error - uint64 addition overflow",
			httpBody: &httpBody{
				addrs: validAddr,
			},
			getBalanceOfAddrsArg: []cipher.Address{address},
			getBalanceOfAddrsResponse: []wallet.BalancePair{
				{
					Confirmed: wallet.Balance{Coins: 0, Hours: 0},
					Predicted: wallet.Balance{Coins: math.MaxInt64 + 1, Hours: 0},
				},
				{
					Confirmed: wallet.Balance{Coins: 0, Hours: 0},
					Predicted: wallet.Balance{Coins: math.MaxInt64 + 1, Hours: 0},
				},
			},
		},
		{
			name:   "200 - OK",
			method: http.MethodGet,
			status: http.StatusOK,
			err:    "200 - OK",
			httpBody: &httpBody{
				addrs: validAddr,
			},
			getBalanceOfAddrsArg: []cipher.Address{address},
			getBalanceOfAddrsResponse: []wallet.BalancePair{
				{
					Confirmed: wallet.Balance{Coins: 0, Hours: 0},
					Predicted: wallet.Balance{Coins: 0, Hours: 0},
				},
				{
					Confirmed: wallet.Balance{Coins: 0, Hours: 0},
					Predicted: wallet.Balance{Coins: 0, Hours: 0},
				},
			},
			httpResponse: readable.BalancePair{},
		},
		{
			name:   "200 - OK POST",
			method: http.MethodPost,
			status: http.StatusOK,
			err:    "200 - OK",
			httpBody: &httpBody{
				addrs: validAddr,
			},
			getBalanceOfAddrsArg: []cipher.Address{address},
			getBalanceOfAddrsResponse: []wallet.BalancePair{
				{
					Confirmed: wallet.Balance{Coins: 0, Hours: 0},
					Predicted: wallet.Balance{Coins: 0, Hours: 0},
				},
				{
					Confirmed: wallet.Balance{Coins: 0, Hours: 0},
					Predicted: wallet.Balance{Coins: 0, Hours: 0},
				},
			},
			httpResponse: readable.BalancePair{},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}
			endpoint := "/api/v1/balance"
			gateway.On("GetBalanceOfAddresses", tc.getBalanceOfAddrsArg).Return(tc.getBalanceOfAddrsResponse, tc.getBalanceOfAddrsError)

			v := url.Values{}
			if tc.httpBody != nil {
				if tc.httpBody.addrs != "" {
					v.Add("addrs", tc.httpBody.addrs)
				}
			}

			var reqBody io.Reader
			if len(v) > 0 {
				if tc.method == http.MethodPost {
					reqBody = strings.NewReader(v.Encode())
				} else {
					endpoint += "?" + v.Encode()
				}
			}

			req, err := http.NewRequest(tc.method, endpoint, reqBody)
			require.NoError(t, err)

			if tc.method == http.MethodPost {
				req.Header.Set("Content-Type", ContentTypeForm)
			}

			rr := httptest.NewRecorder()

			handler := newServerMux(defaultMuxConfig(), gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "got `%v` want `%v`", status, tc.status)

			if status != http.StatusOK {
				require.Equal(t, tc.err, strings.TrimSpace(rr.Body.String()))
			} else {
				var msg readable.BalancePair
				err = json.Unmarshal(rr.Body.Bytes(), &msg)
				require.NoError(t, err)
				require.Equal(t, tc.httpResponse, msg, tc.name)
			}
		})
	}
}

func TestWalletGet(t *testing.T) {
	_, resEntries := makeEntries([]byte("seed"), 5)
	type httpBody struct {
		WalletID string
		Dst      string
		Coins    string
	}

	tt := []struct {
		name                       string
		method                     string
		body                       *httpBody
		status                     int
		err                        string
		walletID                   string
		gatewayGetWalletResultFunc func(string) wallet.Wallet
		responseBody               WalletResponse
		gatewayGetWalletErr        error
	}{
		{
			name:     "405",
			method:   http.MethodPost,
			status:   http.StatusMethodNotAllowed,
			err:      "405 Method Not Allowed",
			walletID: "0",
		},
		{
			name:     "400 - no walletID",
			method:   http.MethodGet,
			status:   http.StatusBadRequest,
			err:      "400 Bad Request - missing wallet id",
			walletID: "",
		},
		{
			name:   "400 - error from the `gateway.GetWallet(wltID)`",
			method: http.MethodGet,
			body: &httpBody{
				WalletID: "123",
			},
			status:   http.StatusBadRequest,
			err:      "400 Bad Request - wallet 123 doesn't exist",
			walletID: "123",
			gatewayGetWalletResultFunc: func(_ string) wallet.Wallet {
				var p *deterministic.Wallet
				return p
			},
			gatewayGetWalletErr: errors.New("wallet 123 doesn't exist"),
		},
		{
			name:   "403 - Forbidden - wallet API disabled",
			method: http.MethodGet,
			body: &httpBody{
				WalletID: "1234",
			},
			status:   http.StatusForbidden,
			err:      "403 Forbidden",
			walletID: "1234",
			gatewayGetWalletResultFunc: func(_ string) wallet.Wallet {
				return &deterministic.Wallet{
					Meta: wallet.Meta{"seed": "seed", "lastSeed": "seed"},
				}
			},
			gatewayGetWalletErr: wallet.ErrWalletAPIDisabled,
		},
		{
			name:   "200 - OK",
			method: http.MethodGet,
			body: &httpBody{
				WalletID: "1234",
			},
			status:   http.StatusOK,
			walletID: "1234",
			gatewayGetWalletResultFunc: func(_ string) wallet.Wallet {
				w, err := deterministic.NewWallet(
					"test.wlt",
					"test", "seed",
					wallet.OptionGenerateN(5))
				require.NoError(t, err)
				w.SetTimestamp(0)
				return w
			},
			responseBody: WalletResponse{
				Meta: readable.WalletMeta{
					Coin:       "skycoin",
					Filename:   "test.wlt",
					Type:       "deterministic",
					Label:      "test",
					Version:    "0.4",
					CryptoType: "scrypt-chacha20poly1305",
					Encrypted:  false,
				},
				Entries: resEntries[:],
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}
			gateway.On("GetWallet", tc.walletID).Return(tc.gatewayGetWalletResultFunc, tc.gatewayGetWalletErr)

			v := url.Values{}

			endpoint := "/api/v1/wallet"

			if tc.body != nil {
				if tc.body.WalletID != "" {
					v.Add("id", tc.body.WalletID)
				}
			}

			if len(v) > 0 {
				endpoint += "?" + v.Encode()
			}

			req, err := http.NewRequest(tc.method, endpoint, nil)
			require.NoError(t, err)

			setCSRFParameters(t, tokenValid, req)

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = false

			handler := newServerMux(cfg, gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "got `%v` want `%v`", status, tc.status)

			if status != http.StatusOK {
				require.Equal(t, tc.err, strings.TrimSpace(rr.Body.String()),
					"got `%v`| %d, want `%v`",
					strings.TrimSpace(rr.Body.String()), status, tc.err)
			} else {
				var rlt WalletResponse
				err = json.Unmarshal(rr.Body.Bytes(), &rlt)
				require.NoError(t, err)
				require.Equal(t, tc.responseBody, rlt)
			}
		})
	}
}

func TestWalletBalanceHandler(t *testing.T) {
	type httpBody struct {
		WalletID string
		Dst      string
		Coins    string
	}

	type balanceResult struct {
		BalancePair wallet.BalancePair
		Addresses   wallet.AddressBalances
	}

	tt := []struct {
		name                          string
		method                        string
		body                          *httpBody
		status                        int
		err                           string
		walletID                      string
		gatewayGetWalletBalanceResult balanceResult
		gatewayBalanceErr             error
		result                        *readable.BalancePair
	}{
		{
			name:     "405",
			method:   http.MethodPost,
			status:   http.StatusMethodNotAllowed,
			err:      "405 Method Not Allowed",
			walletID: "0",
		},
		{
			name:     "400 - no walletID",
			method:   http.MethodGet,
			status:   http.StatusBadRequest,
			err:      "400 Bad Request - missing wallet id",
			walletID: "0",
		},
		{
			name:   "404 - gw `wallet doesn't exist` error",
			method: http.MethodGet,
			body: &httpBody{
				WalletID: "notFoundId",
			},
			status:                        http.StatusNotFound,
			err:                           "404 Not Found",
			walletID:                      "notFoundId",
			gatewayGetWalletBalanceResult: balanceResult{},
			gatewayBalanceErr:             wallet.ErrWalletNotExist,
			result: &readable.BalancePair{
				Confirmed: readable.Balance{Coins: 0, Hours: 0},
				Predicted: readable.Balance{Coins: 0, Hours: 0},
			},
		},
		{
			name:   "500 - gw other error",
			method: http.MethodGet,
			body: &httpBody{
				WalletID: "someId",
			},
			status:                        http.StatusInternalServerError,
			err:                           "500 Internal Server Error - gatewayBalanceError",
			walletID:                      "someId",
			gatewayGetWalletBalanceResult: balanceResult{},
			gatewayBalanceErr:             errors.New("gatewayBalanceError"),
			result: &readable.BalancePair{
				Confirmed: readable.Balance{Coins: 0, Hours: 0},
				Predicted: readable.Balance{Coins: 0, Hours: 0},
			},
		},
		{
			name:   "403 - Forbidden - wallet API disabled",
			method: http.MethodGet,
			body: &httpBody{
				WalletID: "foo",
			},
			status:                        http.StatusForbidden,
			err:                           "403 Forbidden",
			walletID:                      "foo",
			gatewayGetWalletBalanceResult: balanceResult{},
			gatewayBalanceErr:             wallet.ErrWalletAPIDisabled,
		},
		{
			name:   "200 - OK",
			method: http.MethodGet,
			body: &httpBody{
				WalletID: "foo",
			},
			status:   http.StatusOK,
			err:      "",
			walletID: "foo",
			result:   &readable.BalancePair{},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}
			gateway.On("GetWalletBalance", tc.walletID).Return(tc.gatewayGetWalletBalanceResult.BalancePair,
				tc.gatewayGetWalletBalanceResult.Addresses, tc.gatewayBalanceErr)

			endpoint := "/api/v1/wallet/balance"

			v := url.Values{}
			if tc.body != nil {
				if tc.body.WalletID != "" {
					v.Add("id", tc.body.WalletID)
				}
			}
			if len(v) > 0 {
				endpoint += "?" + v.Encode()
			}
			req, err := http.NewRequest(tc.method, endpoint, strings.NewReader(v.Encode()))
			require.NoError(t, err)
			req.Header.Add("Content-Type", ContentTypeForm)

			setCSRFParameters(t, tokenValid, req)

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = false

			handler := newServerMux(cfg, gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "got `%v` want `%v`", status, tc.status)
			if status != tc.status {
				t.Errorf("got `%v` want `%v`", status, tc.status)
			}
			if status != http.StatusOK {
				require.Equal(t, tc.err, strings.TrimSpace(rr.Body.String()), "got `%v`| %d, want `%v`",
					strings.TrimSpace(rr.Body.String()), status, tc.err)
			} else {
				var msg readable.BalancePair
				err = json.Unmarshal(rr.Body.Bytes(), &msg)
				require.NoError(t, err)
				require.Equal(t, tc.result, &msg, tc.name)
			}
		})
	}
}

func TestUpdateWalletLabelHandler(t *testing.T) {
	type httpBody struct {
		WalletID string
		Label    string
	}

	tt := []struct {
		name                        string
		method                      string
		body                        *httpBody
		status                      int
		err                         string
		walletID                    string
		label                       string
		gatewayUpdateWalletLabelErr error
		responseBody                string
	}{
		{
			name:   "405",
			method: http.MethodGet,
			body:   &httpBody{},
			status: http.StatusMethodNotAllowed,
			err:    "405 Method Not Allowed",
		},
		{
			name:   "400 - missing wallet id",
			method: http.MethodPost,
			body:   &httpBody{},
			status: http.StatusBadRequest,
			err:    "400 Bad Request - missing wallet id",
		},
		{
			name:   "400 - missing label",
			method: http.MethodPost,
			body: &httpBody{
				WalletID: "foo",
			},
			status:   http.StatusBadRequest,
			err:      "400 Bad Request - missing label",
			walletID: "foo",
		},
		{
			name:   "404 - gateway.UpdateWalletLabel ErrWalletNotExist",
			method: http.MethodPost,
			body: &httpBody{
				WalletID: "foo",
				Label:    "label",
			},
			status:                      http.StatusNotFound,
			err:                         "404 Not Found",
			walletID:                    "foo",
			label:                       "label",
			gatewayUpdateWalletLabelErr: wallet.ErrWalletNotExist,
		},
		{
			name:   "500 - gateway.UpdateWalletLabel error",
			method: http.MethodPost,
			body: &httpBody{
				WalletID: "foo",
				Label:    "label",
			},
			status:                      http.StatusInternalServerError,
			err:                         "500 Internal Server Error - gateway.UpdateWalletLabel error",
			walletID:                    "foo",
			label:                       "label",
			gatewayUpdateWalletLabelErr: errors.New("gateway.UpdateWalletLabel error"),
		},
		{
			name:   "403 Forbidden - wallet API disabled",
			method: http.MethodPost,
			body: &httpBody{
				WalletID: "foo",
				Label:    "label",
			},
			status:                      http.StatusForbidden,
			err:                         "403 Forbidden",
			walletID:                    "foo",
			label:                       "label",
			gatewayUpdateWalletLabelErr: wallet.ErrWalletAPIDisabled,
		},
		{
			name:   "200 OK",
			method: http.MethodPost,
			body: &httpBody{
				WalletID: "foo",
				Label:    "label",
			},
			status:                      http.StatusOK,
			err:                         "",
			walletID:                    "foo",
			label:                       "label",
			gatewayUpdateWalletLabelErr: nil,
			responseBody:                "\"success\"",
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}
			gateway.On("UpdateWalletLabel", tc.walletID, tc.label).Return(tc.gatewayUpdateWalletLabelErr)

			endpoint := "/api/v1/wallet/update"

			v := url.Values{}
			if tc.body != nil {
				if tc.body.WalletID != "" {
					v.Add("id", tc.body.WalletID)
				}
				if tc.body.Label != "" {
					v.Add("label", tc.body.Label)
				}
			}

			req, err := http.NewRequest(tc.method, endpoint, strings.NewReader(v.Encode()))
			require.NoError(t, err)
			req.Header.Add("Content-Type", ContentTypeForm)

			setCSRFParameters(t, tokenValid, req)

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = false

			handler := newServerMux(cfg, gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "got `%v` want `%v`", status, tc.status)

			if status != http.StatusOK {
				require.Equal(t, tc.err, strings.TrimSpace(rr.Body.String()), "got `%v`| %d, want `%v`",
					strings.TrimSpace(rr.Body.String()), status, tc.err)
			} else {
				require.Equal(t, tc.responseBody, rr.Body.String(), tc.name)
			}
		})
	}
}

func TestWalletTransactionsHandler(t *testing.T) {
	type httpBody struct {
		walletID string
		verbose  string
	}

	uTxn := &visor.UnconfirmedTransaction{
		Transaction: coin.Transaction{
			In: []cipher.SHA256{testutil.RandSHA256(t)},
		},
	}

	unconfirmedTxn, err := readable.NewUnconfirmedTransaction(uTxn)
	require.NoError(t, err)

	unconfirmedTxnVerbose, err := readable.NewUnconfirmedTransactionVerbose(uTxn, []visor.TransactionInput{
		visor.TransactionInput{},
	})
	require.NoError(t, err)

	tt := []struct {
		name                                         string
		method                                       string
		body                                         *httpBody
		status                                       int
		err                                          string
		walletID                                     string
		verbose                                      bool
		gatewayGetWalletUnconfirmedTxnsResult        []visor.UnconfirmedTransaction
		gatewayGetWalletUnconfirmedTxnsErr           error
		gatewayGetWalletUnconfirmedTxnsVerboseResult []readable.UnconfirmedTransactionVerbose
		gatewayGetWalletUnconfirmedTxnsVerboseErr    error
		responseBody                                 interface{}
	}{
		{
			name:   "405",
			method: http.MethodPost,
			status: http.StatusMethodNotAllowed,
			err:    "405 Method Not Allowed",
		},

		{
			name:   "400 - missing wallet id",
			method: http.MethodGet,
			status: http.StatusBadRequest,
			err:    "400 Bad Request - missing wallet id",
		},

		{
			name:   "400 - invalid verbose",
			method: http.MethodGet,
			status: http.StatusBadRequest,
			body: &httpBody{
				verbose: "foo",
			},
			err: "400 Bad Request - Invalid value for verbose",
		},

		{
			name:   "500 - gateway.GetWalletUnconfirmedTransactions error",
			method: http.MethodGet,
			body: &httpBody{
				walletID: "foo",
			},
			status:                             http.StatusInternalServerError,
			err:                                "500 Internal Server Error - gateway.GetWalletUnconfirmedTransactions error",
			walletID:                           "foo",
			gatewayGetWalletUnconfirmedTxnsErr: errors.New("gateway.GetWalletUnconfirmedTransactions error"),
		},

		{
			name:   "500 - gateway.GetWalletUnconfirmedTransactionsVerbose error",
			method: http.MethodGet,
			body: &httpBody{
				walletID: "foo",
				verbose:  "1",
			},
			verbose:  true,
			status:   http.StatusInternalServerError,
			err:      "500 Internal Server Error - gateway.GetWalletUnconfirmedTransactionsVerbose error",
			walletID: "foo",
			gatewayGetWalletUnconfirmedTxnsVerboseErr: errors.New("gateway.GetWalletUnconfirmedTransactionsVerbose error"),
		},

		{
			name:   "404 - wallet doesn't exist",
			method: http.MethodGet,
			body: &httpBody{
				walletID: "foo",
			},
			status:                             http.StatusNotFound,
			err:                                "404 Not Found",
			walletID:                           "foo",
			gatewayGetWalletUnconfirmedTxnsErr: wallet.ErrWalletNotExist,
		},

		{
			name:   "404 - wallet doesn't exist verbose",
			method: http.MethodGet,
			body: &httpBody{
				walletID: "foo",
				verbose:  "1",
			},
			verbose:  true,
			status:   http.StatusNotFound,
			err:      "404 Not Found",
			walletID: "foo",
			gatewayGetWalletUnconfirmedTxnsVerboseErr: wallet.ErrWalletNotExist,
		},

		{
			name:   "403 - Forbidden - wallet API disabled",
			method: http.MethodGet,
			body: &httpBody{
				walletID: "foo",
			},
			status:                             http.StatusForbidden,
			err:                                "403 Forbidden",
			walletID:                           "foo",
			gatewayGetWalletUnconfirmedTxnsErr: wallet.ErrWalletAPIDisabled,
		},

		{
			name:   "403 - Forbidden - wallet API disabled verbose",
			method: http.MethodGet,
			body: &httpBody{
				walletID: "foo",
				verbose:  "1",
			},
			verbose:  true,
			status:   http.StatusForbidden,
			err:      "403 Forbidden",
			walletID: "foo",
			gatewayGetWalletUnconfirmedTxnsVerboseErr: wallet.ErrWalletAPIDisabled,
		},

		{
			name:   "200 - OK",
			method: http.MethodGet,
			body: &httpBody{
				walletID: "foo",
			},
			status:                                http.StatusOK,
			walletID:                              "foo",
			gatewayGetWalletUnconfirmedTxnsResult: make([]visor.UnconfirmedTransaction, 1),
			responseBody: UnconfirmedTxnsResponse{
				Transactions: []readable.UnconfirmedTransactions{
					*unconfirmedTxn,
				},
			},
		},

		{
			name:   "200 - OK verbose",
			method: http.MethodGet,
			body: &httpBody{
				walletID: "foo",
				verbose:  "1",
			},
			verbose:  true,
			status:   http.StatusOK,
			walletID: "foo",
			gatewayGetWalletUnconfirmedTxnsVerboseResult: make([]readable.UnconfirmedTransactionVerbose, 1),
			responseBody: UnconfirmedTxnsVerboseResponse{
				Transactions: []readable.UnconfirmedTransactionVerbose{
					*unconfirmedTxnVerbose,
				},
			},
		},
	}

	for _, tc := range tt {
		gateway := &MockGatewayer{}
		gateway.On("GetWalletUnconfirmedTransactions", tc.walletID).Return(tc.gatewayGetWalletUnconfirmedTxnsResult, tc.gatewayGetWalletUnconfirmedTxnsErr)
		gateway.On("GetWalletUnconfirmedTransactionsVerbose", tc.walletID).Return(tc.gatewayGetWalletUnconfirmedTxnsVerboseResult, tc.gatewayGetWalletUnconfirmedTxnsVerboseErr)

		endpoint := "/api/v1/wallet/transactions"

		v := url.Values{}
		if tc.body != nil {
			if tc.body.walletID != "" {
				v.Add("id", tc.body.walletID)
			}
		}
		if len(v) > 0 {
			endpoint += "?" + v.Encode()
		}
		req, err := http.NewRequest(tc.method, endpoint, nil)
		require.NoError(t, err)

		setCSRFParameters(t, tokenValid, req)

		rr := httptest.NewRecorder()

		cfg := defaultMuxConfig()
		cfg.disableCSRF = false

		handler := newServerMux(cfg, gateway)
		handler.ServeHTTP(rr, req)

		status := rr.Code
		require.Equal(t, tc.status, status, "got `%v` want `%v`",
			tc.name, status, tc.status)

		if status != http.StatusOK {
			require.Equal(t, tc.err, strings.TrimSpace(rr.Body.String()), "got `%v`| %d, want `%v`",
				strings.TrimSpace(rr.Body.String()), status, tc.err)
			return
		}

		if tc.verbose {
			var msg UnconfirmedTxnsVerboseResponse
			err = json.Unmarshal(rr.Body.Bytes(), &msg)
			require.NoError(t, err)
			// require.Equal on whole response might result in flaky tests as there is a time field attached to unconfirmed txn response
			require.IsType(t, msg, tc.responseBody)
			require.Len(t, msg.Transactions, 1)
			require.Equal(t, msg.Transactions[0].Transaction, tc.responseBody.(UnconfirmedTxnsVerboseResponse).Transactions[0].Transaction)
		} else {
			var msg UnconfirmedTxnsResponse
			err = json.Unmarshal(rr.Body.Bytes(), &msg)
			require.NoError(t, err)
			// require.Equal on whole response might result in flaky tests as there is a time field attached to unconfirmed txn response
			require.IsType(t, msg, tc.responseBody)
			require.Len(t, msg.Transactions, 1)
			require.Equal(t, msg.Transactions[0].Transaction, tc.responseBody.(UnconfirmedTxnsResponse).Transactions[0].Transaction)
		}
	}
}

func TestWalletCreateHandler(t *testing.T) {
	_, responseEntries := makeEntries([]byte("seed"), 5)
	type httpBody struct {
		Seed           string
		Label          string
		ScanN          string
		Encrypt        bool
		Password       string
		Type           string
		SeedPassphrase string
		Bip44Coin      string
		XPub           string
	}
	tt := []struct {
		name                      string
		method                    string
		body                      *httpBody
		status                    int
		err                       string
		wltName                   string
		options                   wallet.Options
		gatewayCreateWalletResult func(string, wallet.Options) wallet.Wallet
		gatewayCreateWalletErr    error
		responseBody              WalletResponse
		csrfDisabled              bool
	}{
		{
			name:    "405",
			method:  http.MethodGet,
			status:  http.StatusMethodNotAllowed,
			err:     "405 Method Not Allowed",
			wltName: "foo",
		},
		{
			name:    "400 - missing type",
			method:  http.MethodPost,
			body:    &httpBody{},
			status:  http.StatusBadRequest,
			err:     "400 Bad Request - missing type",
			wltName: "foo",
		},
		{
			name:   "400 - missing seed",
			method: http.MethodPost,
			body: &httpBody{
				Type:  wallet.WalletTypeDeterministic,
				ScanN: "1",
			},
			options: wallet.Options{
				Type:     wallet.WalletTypeDeterministic,
				Password: []byte{},
				ScanN:    1,
			},
			status:                 http.StatusBadRequest,
			gatewayCreateWalletErr: wallet.ErrMissingSeed,
			gatewayCreateWalletResult: func(_ string, _ wallet.Options) wallet.Wallet {
				var p *deterministic.Wallet
				return p
			},
			err:     "400 Bad Request - missing seed",
			wltName: "foo",
		},
		{
			name:   "400 - missing label",
			method: http.MethodPost,
			body: &httpBody{
				Type:  wallet.WalletTypeDeterministic,
				Seed:  "foo",
				ScanN: "1",
			},
			options: wallet.Options{
				Type:     wallet.WalletTypeDeterministic,
				Seed:     "foo",
				Password: []byte{},
				ScanN:    1,
			},
			status: http.StatusBadRequest,

			gatewayCreateWalletErr: wallet.ErrMissingLabel,
			gatewayCreateWalletResult: func(_ string, _ wallet.Options) wallet.Wallet {
				var p *deterministic.Wallet
				return p
			},
			err:     "400 Bad Request - missing label",
			wltName: "foo",
		},
		{
			name:   "400 - invalid scan value",
			method: http.MethodPost,
			body: &httpBody{
				Type:  wallet.WalletTypeDeterministic,
				Seed:  "foo",
				Label: "bar",
				ScanN: "bad scanN",
			},
			status:  http.StatusBadRequest,
			err:     "400 Bad Request - invalid scan value",
			wltName: "foo",
		},
		{
			name:   "400 - invalid bip44 coin",
			method: http.MethodPost,
			body: &httpBody{
				Type:      wallet.WalletTypeBip44,
				Seed:      bip39.MustNewDefaultMnemonic(),
				Label:     "bar",
				ScanN:     "1",
				Bip44Coin: "foo",
			},
			status:  http.StatusBadRequest,
			err:     "400 Bad Request - invalid bip44-coin value",
			wltName: "foo",
		},
		{
			name:   "400 - invalid bip44 coin 2",
			method: http.MethodPost,
			body: &httpBody{
				Type:      wallet.WalletTypeBip44,
				Seed:      bip39.MustNewDefaultMnemonic(),
				Label:     "bar",
				ScanN:     "1",
				Bip44Coin: "4294967296", // MaxUint32+1: Bip44Coin must be uint32
			},
			status:  http.StatusBadRequest,
			err:     "400 Bad Request - invalid bip44-coin value",
			wltName: "foo",
		},
		{
			name:   "400 - bip44 coin does not match type",
			method: http.MethodPost,
			body: &httpBody{
				Type:      wallet.WalletTypeDeterministic,
				Seed:      bip39.MustNewDefaultMnemonic(),
				Label:     "bar",
				ScanN:     "1",
				Bip44Coin: "8000",
			},
			status:  http.StatusBadRequest,
			err:     "400 Bad Request - bip44-coin is only valid for bip44 type wallets",
			wltName: "foo",
		},
		{
			name:   "400 - seed in use",
			method: http.MethodPost,
			body: &httpBody{
				Type:  wallet.WalletTypeDeterministic,
				Seed:  "foo",
				Label: "bar",
				ScanN: "1",
			},
			status: http.StatusBadRequest,
			err:    "400 Bad Request - a wallet already exists with this seed",
			options: wallet.Options{
				Type:     wallet.WalletTypeDeterministic,
				Label:    "bar",
				Seed:     "foo",
				ScanN:    1,
				Password: []byte{},
			},
			gatewayCreateWalletErr: wallet.ErrSeedUsed,
			gatewayCreateWalletResult: func(_ string, _ wallet.Options) wallet.Wallet {
				var p *deterministic.Wallet
				return p
			},
		},
		{
			name:   "500 - gateway.CreateWallet error",
			method: http.MethodPost,
			body: &httpBody{
				Type:  wallet.WalletTypeDeterministic,
				Seed:  "foo",
				Label: "bar",
				ScanN: "1",
			},
			status: http.StatusInternalServerError,
			err:    "500 Internal Server Error - gateway.CreateWallet error",
			options: wallet.Options{
				Type:     wallet.WalletTypeDeterministic,
				Label:    "bar",
				Seed:     "foo",
				ScanN:    1,
				Password: []byte{},
			},
			gatewayCreateWalletErr: errors.New("gateway.CreateWallet error"),
			gatewayCreateWalletResult: func(_ string, _ wallet.Options) wallet.Wallet {
				var p *deterministic.Wallet
				return p
			},
		},
		{
			name:   "403 - Forbidden - wallet API disabled",
			method: http.MethodPost,
			body: &httpBody{
				Type:  wallet.WalletTypeDeterministic,
				Seed:  "foo",
				Label: "bar",
				ScanN: "2",
			},
			status:  http.StatusForbidden,
			err:     "403 Forbidden",
			wltName: "filename",
			options: wallet.Options{
				Type:     wallet.WalletTypeDeterministic,
				Label:    "bar",
				Seed:     "foo",
				Password: []byte{},
				ScanN:    2,
			},
			gatewayCreateWalletErr: wallet.ErrWalletAPIDisabled,
			gatewayCreateWalletResult: func(_ string, _ wallet.Options) wallet.Wallet {
				var p *deterministic.Wallet
				return p
			},
		},
		{
			name:   "200 - OK",
			method: http.MethodPost,
			body: &httpBody{
				Type:  wallet.WalletTypeDeterministic,
				Seed:  "foo",
				Label: "bar",
				ScanN: "2",
			},
			status:  http.StatusOK,
			err:     "",
			wltName: "filename",
			options: wallet.Options{
				Type:     wallet.WalletTypeDeterministic,
				Label:    "bar",
				Seed:     "foo",
				Password: []byte{},
				ScanN:    2,
			},
			gatewayCreateWalletResult: func(_ string, _ wallet.Options) wallet.Wallet {
				w, err := deterministic.NewWallet(
					"filename",
					"test",
					"seed",
					wallet.OptionGenerateN(5),
				)
				w.SetTimestamp(0)
				require.NoError(t, err)
				return w
			},
			responseBody: WalletResponse{
				Meta: readable.WalletMeta{
					Coin:       "skycoin",
					Label:      "test",
					Filename:   "filename",
					Type:       "deterministic",
					Version:    "0.4",
					CryptoType: "scrypt-chacha20poly1305",
				},
				Entries: responseEntries[:],
			},
		},
		{
			name:   "200 - OK - with seed passphrase",
			method: http.MethodPost,
			body: &httpBody{
				Type:           wallet.WalletTypeBip44,
				Seed:           "foo",
				Label:          "bar",
				ScanN:          "2",
				SeedPassphrase: "foobar",
			},
			status:  http.StatusOK,
			err:     "",
			wltName: "filename",
			options: wallet.Options{
				Type:           wallet.WalletTypeBip44,
				Label:          "bar",
				Seed:           "foo",
				Password:       []byte{},
				ScanN:          2,
				SeedPassphrase: "foobar",
			},
			gatewayCreateWalletResult: func(_ string, _ wallet.Options) wallet.Wallet {
				w, err := deterministic.NewWallet(
					"filename",
					"test",
					"seed",
					wallet.OptionGenerateN(5),
				)
				require.NoError(t, err)
				w.SetTimestamp(0)
				return w
			},
			responseBody: WalletResponse{
				Meta: readable.WalletMeta{
					Coin:       "skycoin",
					Label:      "test",
					Filename:   "filename",
					Type:       "deterministic",
					Version:    "0.4",
					CryptoType: "scrypt-chacha20poly1305",
				},
				Entries: responseEntries[:],
			},
		},
		{
			name:   "200 - OK - xpub",
			method: http.MethodPost,
			body: &httpBody{
				Type:  wallet.WalletTypeXPub,
				Label: "bar",
				ScanN: "2",
				XPub:  "xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet8",
			},
			status:  http.StatusOK,
			err:     "",
			wltName: "filename",
			options: wallet.Options{
				Type:     wallet.WalletTypeXPub,
				Label:    "bar",
				Password: []byte{},
				ScanN:    2,
				XPub:     "xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet8",
			},
			gatewayCreateWalletResult: func(_ string, _ wallet.Options) wallet.Wallet {
				w, err := deterministic.NewWallet(
					"filename",
					"test",
					"seed",
					wallet.OptionGenerateN(5),
				)
				require.NoError(t, err)
				w.SetTimestamp(0)
				return w
			},
			responseBody: WalletResponse{
				Meta: readable.WalletMeta{
					Coin:       "skycoin",
					Label:      "test",
					Filename:   "filename",
					Type:       "deterministic",
					Version:    "0.4",
					CryptoType: "scrypt-chacha20poly1305",
				},
				Entries: responseEntries[:],
			},
		},
		// CSRF Tests
		{
			name:   "200 - OK - CSRF disabled",
			method: http.MethodPost,
			body: &httpBody{
				Type:  wallet.WalletTypeDeterministic,
				Seed:  "foo",
				Label: "bar",
				ScanN: "2",
			},
			status:  http.StatusOK,
			err:     "",
			wltName: "filename",
			options: wallet.Options{
				Type:     wallet.WalletTypeDeterministic,
				Label:    "bar",
				Seed:     "foo",
				Password: []byte{},
				ScanN:    2,
			},
			gatewayCreateWalletResult: func(_ string, _ wallet.Options) wallet.Wallet {
				return &deterministic.Wallet{
					Meta: wallet.Meta{
						"filename": "filename",
					},
				}
			},
			responseBody: WalletResponse{
				Meta: readable.WalletMeta{
					Filename: "filename",
				},
				Entries: []readable.WalletEntry{},
			},
			csrfDisabled: true,
		},
		{
			name:   "200 - OK - Encrypted",
			method: http.MethodPost,
			body: &httpBody{
				Type:     wallet.WalletTypeDeterministic,
				Seed:     "foo",
				Label:    "bar",
				Encrypt:  true,
				Password: "pwd",
				ScanN:    "2",
			},
			status:  http.StatusOK,
			err:     "",
			wltName: "filename",
			options: wallet.Options{
				Type:     wallet.WalletTypeDeterministic,
				Label:    "bar",
				Seed:     "foo",
				Encrypt:  true,
				Password: []byte("pwd"),
				ScanN:    2,
			},
			gatewayCreateWalletResult: func(_ string, _ wallet.Options) wallet.Wallet {
				return &deterministic.Wallet{
					Meta: wallet.Meta{
						"filename":  "filename",
						"label":     "bar",
						"encrypted": "true",
						"secrets":   "secrets",
					},
				}
			},
			responseBody: WalletResponse{
				Meta: readable.WalletMeta{
					Filename:  "filename",
					Label:     "bar",
					Encrypted: true,
				},
				Entries: []readable.WalletEntry{},
			},
		},
		{
			name:   "400 Bad request - encrypt without password",
			method: http.MethodPost,
			body: &httpBody{
				Type:    wallet.WalletTypeDeterministic,
				Seed:    "foo",
				Label:   "bar",
				Encrypt: true,
			},
			status: http.StatusBadRequest,
			err:    "400 Bad Request - missing password",
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}
			gateway.On("TransactionsFinder").Return(&visor.TransactionsFinder{})
			tc.options.TF = gateway.TransactionsFinder()
			gateway.On("CreateWallet", "", tc.options).Return(tc.gatewayCreateWalletResult, tc.gatewayCreateWalletErr)

			endpoint := "/api/v1/wallet/create"

			v := url.Values{}
			if tc.body != nil {
				if tc.body.Seed != "" {
					v.Add("seed", tc.body.Seed)
				}
				if tc.body.Label != "" {
					v.Add("label", tc.body.Label)
				}
				if tc.body.ScanN != "" {
					v.Add("scan", tc.body.ScanN)
				}

				if tc.body.Encrypt {
					v.Add("encrypt", strconv.FormatBool(tc.body.Encrypt))
				}

				if tc.body.Password != "" {
					v.Add("password", tc.body.Password)
				}

				if tc.body.Type != "" {
					v.Add("type", tc.body.Type)
				}

				if tc.body.SeedPassphrase != "" {
					v.Add("seed-passphrase", tc.body.SeedPassphrase)
				}

				if tc.body.Bip44Coin != "" {
					v.Add("bip44-coin", tc.body.Bip44Coin)
				}

				if tc.body.XPub != "" {
					v.Add("xpub", tc.body.XPub)
				}
			}

			req, err := http.NewRequest(tc.method, endpoint, strings.NewReader(v.Encode()))
			req.Header.Add("Content-Type", ContentTypeForm)
			require.NoError(t, err)

			if tc.csrfDisabled {
				setCSRFParameters(t, tokenInvalid, req)
			} else {
				setCSRFParameters(t, tokenValid, req)
			}

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = tc.csrfDisabled

			handler := newServerMux(cfg, gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "got `%v` want `%v`", status, tc.status)

			if status != http.StatusOK {
				body := strings.TrimSpace(rr.Body.String())
				require.Equal(t, tc.err, body, "got `%v`| %d, want `%v`", body, status, tc.err)
				return
			}

			var msg WalletResponse
			err = json.Unmarshal(rr.Body.Bytes(), &msg)
			require.NoError(t, err)
			require.Equal(t, tc.responseBody, msg, tc.name)
		})
	}
}

func TestWalletNewSeed(t *testing.T) {
	type httpBody struct {
		Entropy string
	}
	tt := []struct {
		name      string
		method    string
		body      *httpBody
		status    int
		err       string
		entropy   string
		resultLen int
	}{
		{
			name:   "405",
			method: http.MethodPut,
			status: http.StatusMethodNotAllowed,
			err:    "405 Method Not Allowed",
		},
		{
			name:   "400 - invalid entropy type",
			method: http.MethodGet,
			body: &httpBody{
				Entropy: "xx",
			},
			status:  http.StatusBadRequest,
			err:     "400 Bad Request - invalid entropy",
			entropy: "xx",
		},
		{
			name:   "400 - `wrong entropy length` error",
			method: http.MethodGet,
			body: &httpBody{
				Entropy: "200",
			},
			status:  http.StatusBadRequest,
			err:     "400 Bad Request - entropy length must be 128 or 256",
			entropy: "200",
		},
		{
			name:      "200 - OK with no entropy",
			method:    http.MethodGet,
			body:      &httpBody{},
			status:    http.StatusOK,
			entropy:   "128",
			resultLen: 12,
		},
		{
			name:   "200 - OK | 12 word seed",
			method: http.MethodGet,
			body: &httpBody{
				Entropy: "128",
			},
			status:    http.StatusOK,
			entropy:   "128",
			resultLen: 12,
		},
		{
			name:   "200 - OK | 24 word seed",
			method: http.MethodGet,
			body: &httpBody{
				Entropy: "256",
			},
			status:    http.StatusOK,
			entropy:   "256",
			resultLen: 24,
		},
	}

	// Loop over each test case
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}

			endpoint := "/api/v1/wallet/newSeed"

			// Add request parameters to url
			v := url.Values{}
			if tc.body != nil {
				if tc.body.Entropy != "" {
					v.Add("entropy", tc.body.Entropy)
				}
			}
			if len(v) > 0 {
				endpoint += "?" + v.Encode()
			}

			req, err := http.NewRequest(tc.method, endpoint, strings.NewReader(v.Encode()))
			require.NoError(t, err)
			req.Header.Add("Content-Type", ContentTypeForm)

			setCSRFParameters(t, tokenValid, req)

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = false

			handler := newServerMux(cfg, gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "got `%v` expected `%v`", tc.name, status, tc.status)
			if status != tc.status {
				t.Errorf("got `%v` want `%v`", status, tc.status)
			}
			if status != http.StatusOK {
				require.Equal(t, tc.err, strings.TrimSpace(rr.Body.String()), "got `%v`| %d, expected `%v`",
					strings.TrimSpace(rr.Body.String()), status, tc.err)
			} else {
				var msg struct {
					Seed string `json:"seed"`
				}
				err = json.Unmarshal(rr.Body.Bytes(), &msg)
				require.NoError(t, err)
				// check that expected length is equal to response length
				require.Equal(t, tc.resultLen, len(strings.Fields(msg.Seed)), tc.name)
			}
		})
	}
}

func TestVerifySeed(t *testing.T) {
	cases := []struct {
		name         string
		method       string
		status       int
		contentType  string
		csrfDisabled bool
		httpBody     string
		httpResponse HTTPResponse
	}{
		{
			name:         "405",
			method:       http.MethodGet,
			status:       http.StatusMethodNotAllowed,
			httpResponse: NewHTTPErrorResponse(http.StatusMethodNotAllowed, ""),
		},

		{
			name:         "415 - Unsupported Media Type",
			method:       http.MethodPost,
			contentType:  ContentTypeForm,
			status:       http.StatusUnsupportedMediaType,
			httpResponse: NewHTTPErrorResponse(http.StatusUnsupportedMediaType, ""),
		},

		{
			name:         "400 - EOF",
			method:       http.MethodPost,
			contentType:  ContentTypeJSON,
			status:       http.StatusBadRequest,
			httpResponse: NewHTTPErrorResponse(http.StatusBadRequest, "EOF"),
		},
		{
			name:         "400 - Missing Seed",
			method:       http.MethodPost,
			contentType:  ContentTypeJSON,
			status:       http.StatusBadRequest,
			httpBody:     "{}",
			httpResponse: NewHTTPErrorResponse(http.StatusBadRequest, "seed is required"),
		},
		{
			name:        "400 - Invalid seed",
			method:      http.MethodPost,
			contentType: ContentTypeJSON,
			status:      http.StatusUnprocessableEntity,
			httpBody: toJSON(t, VerifySeedRequest{
				Seed: "bag attitude butter flock slab desk ship brain famous scheme clerk",
			}),
			httpResponse: NewHTTPErrorResponse(http.StatusUnprocessableEntity, bip39.ErrInvalidNumberOfWords.Error()),
		},
		{
			name:   "200",
			method: http.MethodPost,
			status: http.StatusOK,
			httpBody: toJSON(t, VerifySeedRequest{
				Seed: "chief stadium sniff exhibit ostrich exit fruit noodle good lava coin supply",
			}),
			httpResponse: HTTPResponse{Data: struct{}{}},
		},
		{
			name:   "200 - csrf disabled",
			method: http.MethodPost,
			status: http.StatusOK,
			httpBody: toJSON(t, VerifySeedRequest{
				Seed: "chief stadium sniff exhibit ostrich exit fruit noodle good lava coin supply",
			}),
			httpResponse: HTTPResponse{Data: struct{}{}},
			csrfDisabled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := "/api/v2/wallet/seed/verify"
			gateway := &MockGatewayer{}

			req, err := http.NewRequest(tc.method, endpoint, strings.NewReader(tc.httpBody))
			require.NoError(t, err)

			contentType := tc.contentType
			if contentType == "" {
				contentType = ContentTypeJSON
			}

			req.Header.Set("Content-Type", contentType)

			if tc.csrfDisabled {
				setCSRFParameters(t, tokenInvalid, req)
			} else {
				setCSRFParameters(t, tokenValid, req)
			}

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = tc.csrfDisabled

			handler := newServerMux(cfg, gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "got `%v` want `%v`", status, tc.status)

			var rsp ReceivedHTTPResponse
			err = json.Unmarshal(rr.Body.Bytes(), &rsp)
			require.NoError(t, err)

			require.Equal(t, tc.httpResponse.Error, rsp.Error)

			if rsp.Data == nil {
				require.Nil(t, tc.httpResponse.Data)
			} else {
				require.NotNil(t, tc.httpResponse.Data)

				var addrRsp struct{}
				err := json.Unmarshal(rsp.Data, &addrRsp)
				require.NoError(t, err)

				require.Equal(t, tc.httpResponse.Data, addrRsp)
			}

		})
	}
}

func TestGetWalletSeed(t *testing.T) {
	tt := []struct {
		name                 string
		method               string
		wltID                string
		password             string
		gatewayReturnArgs    []interface{}
		expectStatus         int
		expectSeed           string
		expectSeedPassphrase string
		expectErr            string
		csrfDisabled         bool
	}{
		{
			name:     "200 - OK",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturnArgs: []interface{}{
				"seed",
				"seed-passphrase",
				nil,
			},
			expectStatus:         http.StatusOK,
			expectSeed:           "seed",
			expectSeedPassphrase: "seed-passphrase",
		},
		{
			name:     "200 - OK - CSRF disabled",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturnArgs: []interface{}{
				"seed",
				"",
				nil,
			},
			expectStatus:         http.StatusOK,
			expectSeed:           "seed",
			expectSeedPassphrase: "",
			csrfDisabled:         true,
		},
		{
			name:     "400 - missing wallet id ",
			method:   http.MethodPost,
			wltID:    "",
			password: "pwd",
			gatewayReturnArgs: []interface{}{
				"seed",
				"",
				nil,
			},
			expectStatus: http.StatusBadRequest,
			expectErr:    "400 Bad Request - missing wallet id",
		},
		{
			name:     "400 - missing password",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "",
			gatewayReturnArgs: []interface{}{
				"",
				"",
				wallet.ErrMissingPassword,
			},
			expectStatus: http.StatusBadRequest,
			expectErr:    "400 Bad Request - missing password",
		},
		{
			name:     "400 Bad Request - Invalid password",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturnArgs: []interface{}{
				"",
				"",
				wallet.ErrInvalidPassword,
			},
			expectStatus: http.StatusBadRequest,
			expectErr:    "400 Bad Request - invalid password",
		},
		{
			name:     "400 - wallet not encrypted",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturnArgs: []interface{}{
				"",
				"",
				wallet.ErrWalletNotEncrypted,
			},
			expectStatus: http.StatusBadRequest,
			expectErr:    "400 Bad Request - wallet is not encrypted",
		},
		{
			name:     "404 - wallet does not exist",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturnArgs: []interface{}{
				"",
				"",
				wallet.ErrWalletNotExist,
			},
			expectStatus: http.StatusNotFound,
			expectErr:    "404 Not Found",
		},
		{
			name:         "405 - Method Not Allowed",
			method:       http.MethodGet,
			expectStatus: http.StatusMethodNotAllowed,
			expectErr:    "405 Method Not Allowed",
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}
			gateway.On("GetWalletSeed", tc.wltID, []byte(tc.password)).Return(tc.gatewayReturnArgs...)

			endpoint := "/api/v1/wallet/seed"

			v := url.Values{}
			v.Add("id", tc.wltID)
			if len(tc.password) > 0 {
				v.Add("password", tc.password)
			}

			req, err := http.NewRequest(tc.method, endpoint, strings.NewReader(v.Encode()))
			require.NoError(t, err)
			req.Header.Add("Content-Type", ContentTypeForm)

			setCSRFParameters(t, tokenValid, req)

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = false

			handler := newServerMux(cfg, gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.expectStatus, status)

			if status != http.StatusOK {
				require.Equal(t, tc.expectErr, strings.TrimSpace(rr.Body.String()))
			} else {
				var r WalletSeedResponse
				err := json.Unmarshal(rr.Body.Bytes(), &r)
				require.NoError(t, err)
				require.Equal(t, tc.expectSeed, r.Seed)
				require.Equal(t, tc.expectSeedPassphrase, r.SeedPassphrase)
			}
		})
	}
}

func TestWalletNewAddressesHandler(t *testing.T) {
	type httpBody struct {
		ID       string
		Num      string
		Password string
	}
	type Addresses struct {
		Address []string `json:"addresses"`
	}

	var responseAddresses = Addresses{}
	var responseEmptyAddresses = Addresses{}

	var emptyAddrs = make([]cipher.Address, 0)
	var addrs = make([]cipher.Address, 3)

	for i := 0; i < 3; i++ {
		pub, _ := cipher.MustGenerateDeterministicKeyPair(cipher.RandByte(32))
		addrs[i] = cipher.AddressFromPubKey(pub)
		responseAddresses.Address = append(responseAddresses.Address, addrs[i].String())
	}

	tt := []struct {
		name                      string
		method                    string
		body                      *httpBody
		status                    int
		err                       string
		walletID                  string
		n                         uint64
		password                  string
		gatewayNewAddressesResult []cipher.Address
		gatewayNewAddressesErr    error
		responseBody              Addresses
		csrfDisabled              bool
	}{
		{
			name:   "405",
			method: http.MethodGet,
			status: http.StatusMethodNotAllowed,
			err:    "405 Method Not Allowed",
		},
		{
			name:   "400 - missing wallet id",
			method: http.MethodPost,
			status: http.StatusBadRequest,
			err:    "400 Bad Request - missing wallet id",
		},
		{
			name:   "400 - invalid num value",
			method: http.MethodPost,
			body: &httpBody{
				ID:  "foo",
				Num: "bar",
			},
			status: http.StatusBadRequest,
			err:    "400 Bad Request - invalid num value",
		},
		{
			name:   "400 - gateway.NewAddresses error",
			method: http.MethodPost,
			body: &httpBody{
				ID:  "foo",
				Num: "1",
			},
			status:                 http.StatusBadRequest,
			err:                    "400 Bad Request - gateway.NewAddresses error",
			walletID:               "foo",
			n:                      1,
			gatewayNewAddressesErr: errors.New("gateway.NewAddresses error"),
		},
		{
			name:   "403 - Forbidden - wallet API disabled",
			method: http.MethodPost,
			body: &httpBody{
				ID:  "foo",
				Num: "1",
			},
			status:                 http.StatusForbidden,
			err:                    "403 Forbidden",
			walletID:               "foo",
			n:                      1,
			gatewayNewAddressesErr: wallet.ErrWalletAPIDisabled,
		},
		{
			name:   "400 Bad Request - missing password",
			method: http.MethodPost,
			body: &httpBody{
				ID:  "foo",
				Num: "1",
			},
			status:                 http.StatusBadRequest,
			err:                    "400 Bad Request - missing password",
			walletID:               "foo",
			n:                      1,
			gatewayNewAddressesErr: wallet.ErrMissingPassword,
		},
		{
			name:   "400 Bad Request - Invalid password",
			method: http.MethodPost,
			body: &httpBody{
				ID:  "foo",
				Num: "1",
			},
			status:                 http.StatusBadRequest,
			err:                    "400 Bad Request - invalid password",
			walletID:               "foo",
			n:                      1,
			gatewayNewAddressesErr: wallet.ErrInvalidPassword,
		},
		{
			name:   "400 Bad Request - permission denied",
			method: http.MethodPost,
			body: &httpBody{
				ID:  "foo",
				Num: "1",
			},
			status:                 http.StatusBadRequest,
			err:                    "400 Bad Request - saving wallet permission denied",
			walletID:               "foo",
			n:                      1,
			gatewayNewAddressesErr: wallet.ErrWalletPermission,
		},
		{
			name:   "200 - OK",
			method: http.MethodPost,
			body: &httpBody{
				ID:  "foo",
				Num: "1",
			},
			status:                    http.StatusOK,
			walletID:                  "foo",
			n:                         1,
			gatewayNewAddressesResult: addrs,
			responseBody:              responseAddresses,
		},
		{
			name:   "200 - OK with password",
			method: http.MethodPost,
			body: &httpBody{
				ID:       "foo",
				Num:      "1",
				Password: "pwd",
			},
			status:                    http.StatusOK,
			walletID:                  "foo",
			n:                         1,
			gatewayNewAddressesResult: addrs,
			responseBody:              responseAddresses,
		},
		{
			name:   "200 - OK empty addresses",
			method: http.MethodPost,
			body: &httpBody{
				ID:  "foo",
				Num: "0",
			},
			status:                    http.StatusOK,
			walletID:                  "foo",
			n:                         0,
			gatewayNewAddressesResult: emptyAddrs,
			responseBody:              responseEmptyAddresses,
		},
		{
			name:   "200 - OK - CSRF disabled",
			method: http.MethodPost,
			body: &httpBody{
				ID:  "foo",
				Num: "1",
			},
			status:                    http.StatusOK,
			walletID:                  "foo",
			n:                         1,
			gatewayNewAddressesResult: addrs,
			responseBody:              responseAddresses,
			csrfDisabled:              true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			mb := mock.MatchedBy(func(option wallet.Option) bool {
				n := wallet.GetGenerateNFromOptions(option)
				return tc.n == n
			})
			gateway := &MockGatewayer{}
			gateway.On("NewAddresses", tc.walletID,
				[]byte(tc.password), mb).Return(tc.gatewayNewAddressesResult, tc.gatewayNewAddressesErr)

			endpoint := "/api/v1/wallet/newAddress"

			v := url.Values{}
			if tc.body != nil {
				if tc.body.ID != "" {
					v.Add("id", tc.body.ID)
				}
				if tc.body.Num != "" {
					v.Add("num", tc.body.Num)
				}
			}

			req, err := http.NewRequest(tc.method, endpoint, strings.NewReader(v.Encode()))
			require.NoError(t, err)
			req.Header.Add("Content-Type", ContentTypeForm)

			if tc.csrfDisabled {
				setCSRFParameters(t, tokenInvalid, req)
			} else {
				setCSRFParameters(t, tokenValid, req)
			}

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = tc.csrfDisabled

			handler := newServerMux(cfg, gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "wrong status code: got `%v` want `%v`", status, tc.status)

			if status != http.StatusOK {
				require.Equal(t, tc.err, strings.TrimSpace(rr.Body.String()), "got `%v`| %d, want `%v`",
					strings.TrimSpace(rr.Body.String()), status, tc.err)
			} else {
				var msg Addresses
				err = json.Unmarshal(rr.Body.Bytes(), &msg)
				require.NoError(t, err)
				require.Equal(t, tc.responseBody, msg, tc.name)
			}
		})
	}
}

func TestWalletScanAddressesHandler(t *testing.T) {
	type httpBody struct {
		ID       string
		Num      string
		Password string
	}

	type responseAddresses struct {
		Addresses []string `json:"addresses"`
	}

	type expect struct {
		status int
		addrs  responseAddresses
		err    string
	}

	const addrsN = 10
	const walletID = "test.wlt"

	addrs := make([]cipher.Address, addrsN)
	addrsStr := make([]string, addrsN)
	for i := 0; i < addrsN; i++ {
		pub, _ := cipher.MustGenerateDeterministicKeyPair(cipher.RandByte(32))
		addrs[i] = cipher.AddressFromPubKey(pub)
		addrsStr[i] = addrs[i].String()
	}

	tt := []struct {
		name                       string
		method                     string
		disableCSRF                bool
		password                   string
		scanN                      uint64
		gatewayScanAddressesResult []cipher.Address
		gatewayScanAddressesErr    error
		body                       *httpBody
		expect                     expect
	}{
		{
			name:   "405 Invalid Method",
			method: "GET",
			expect: expect{
				status: http.StatusMethodNotAllowed,
				err:    "405 Method Not Allowed",
			},
		},
		{
			name:   "400 missing wallet id",
			method: "POST",
			body: &httpBody{
				Num: "1",
			},
			scanN: 1,
			expect: expect{
				status: http.StatusBadRequest,
				err:    "400 Bad Request - missing wallet id",
			},
		},
		{
			name:   "400 Invalid num value",
			method: "POST",
			body: &httpBody{
				ID:  walletID,
				Num: "1.01",
			},
			gatewayScanAddressesResult: nil,
			gatewayScanAddressesErr:    wallet.ErrWalletAPIDisabled,
			expect: expect{
				status: http.StatusBadRequest,
				err:    "400 Bad Request - invalid num value",
			},
		},
		{
			name:   "400 Invalid num value, must be  > 0",
			method: "POST",
			body: &httpBody{
				ID:  walletID,
				Num: "0",
			},
			gatewayScanAddressesResult: nil,
			gatewayScanAddressesErr:    wallet.ErrWalletAPIDisabled,
			expect: expect{
				status: http.StatusBadRequest,
				err:    "400 Bad Request - invalid num value, must be > 0",
			},
		},
		{
			name:   "400 other error",
			method: "POST",
			body: &httpBody{
				ID:  walletID,
				Num: "1",
			},
			scanN:                      1,
			gatewayScanAddressesResult: nil,
			gatewayScanAddressesErr:    errors.New("other error"),
			expect: expect{
				status: http.StatusBadRequest,
				err:    "400 Bad Request - other error",
			},
		},
		{
			name:   "403 Wallet API Disabled",
			method: "POST",
			body: &httpBody{
				ID:  walletID,
				Num: "10",
			},
			scanN:                      10,
			gatewayScanAddressesResult: nil,
			gatewayScanAddressesErr:    wallet.ErrWalletAPIDisabled,
			expect: expect{
				status: http.StatusForbidden,
				err:    "403 Forbidden",
			},
		},
		{
			name:   "scan 0 default to 20 get 0",
			method: "POST",
			body: &httpBody{
				ID: walletID,
			},
			scanN:                      20,
			gatewayScanAddressesResult: nil,
			expect: expect{
				status: http.StatusOK,
				addrs:  responseAddresses{},
			},
		},
		{
			name:   "scan 0 default to 20 get 1",
			method: "POST",
			body: &httpBody{
				ID: walletID,
			},
			scanN:                      20,
			gatewayScanAddressesResult: addrs[:1],
			expect: expect{
				status: http.StatusOK,
				addrs:  responseAddresses{addrsStr[:1]},
			},
		},
		{
			name:   "scan 1 get 0",
			method: "POST",
			body: &httpBody{
				ID:  walletID,
				Num: "1",
			},
			scanN:                      1,
			gatewayScanAddressesResult: nil,
			expect: expect{
				status: http.StatusOK,
				addrs:  responseAddresses{},
			},
		},
		{
			name:   "scan 1 get 1",
			method: "POST",
			body: &httpBody{
				ID:  walletID,
				Num: "1",
			},
			scanN:                      1,
			gatewayScanAddressesResult: addrs[:1],
			expect: expect{
				status: http.StatusOK,
				addrs:  responseAddresses{addrsStr[:1]},
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}
			gateway.On("ScanWalletAddresses", walletID, []byte(tc.password), tc.scanN).Return(tc.gatewayScanAddressesResult, tc.gatewayScanAddressesErr)

			c := newHTTPMockClient(gateway, ContentTypeForm, tc.disableCSRF)

			v := url.Values{}
			if tc.body != nil {
				if tc.body.ID != "" {
					v.Add("id", tc.body.ID)
				}
				if tc.body.Password != "" {
					v.Add("password", tc.body.Password)
				}
				if tc.body.Num != "" {
					v.Add("num", tc.body.Num)
				}
			}

			rsp := c.Do(t, tc.method, "/api/v1/wallet/scan", v.Encode())
			require.Equal(t, tc.expect.status, rsp.Code)

			if rsp.Code != http.StatusOK {
				v := strings.TrimSpace(rsp.Body.String())
				require.Equal(t, tc.expect.err, v)
			} else {
				var rspAddrs responseAddresses
				err := json.NewDecoder(rsp.Body).Decode(&rspAddrs)
				require.NoError(t, err)
				require.Equal(t, tc.expect.addrs, rspAddrs)
			}
		})
	}
}

func TestGetWalletFolderHandler(t *testing.T) {
	tt := []struct {
		name                 string
		method               string
		status               int
		err                  string
		getWalletDirResponse string
		getWalletDirErr      error
		httpResponse         WalletFolder
	}{
		{
			name:   "405",
			method: http.MethodPost,
			status: http.StatusMethodNotAllowed,
			err:    "405 Method Not Allowed",
		},
		{
			name:                 "200",
			method:               http.MethodGet,
			status:               http.StatusOK,
			getWalletDirResponse: "/wallet/folder/address",
			httpResponse: WalletFolder{
				Address: "/wallet/folder/address",
			},
		},
		{
			name:            "403 - wallet API disabled",
			method:          http.MethodGet,
			status:          http.StatusForbidden,
			err:             "403 Forbidden",
			getWalletDirErr: wallet.ErrWalletAPIDisabled,
		},
	}

	for _, tc := range tt {
		gateway := &MockGatewayer{}
		gateway.On("WalletDir").Return(tc.getWalletDirResponse, tc.getWalletDirErr)

		endpoint := "/api/v1/wallets/folderName"

		req, err := http.NewRequest(tc.method, endpoint, nil)
		require.NoError(t, err)

		setCSRFParameters(t, tokenValid, req)

		rr := httptest.NewRecorder()

		cfg := defaultMuxConfig()
		cfg.disableCSRF = false

		handler := newServerMux(cfg, gateway)
		handler.ServeHTTP(rr, req)

		status := rr.Code
		require.Equal(t, tc.status, status, "got `%v` want `%v`",
			tc.name, status, tc.status)

		if status != http.StatusOK {
			require.Equal(t, tc.err, strings.TrimSpace(rr.Body.String()), "got `%v`| %d, want `%v`",
				strings.TrimSpace(rr.Body.String()), status, tc.err)
		} else {
			var msg WalletFolder
			err := json.Unmarshal(rr.Body.Bytes(), &msg)
			require.NoError(t, err)
			require.Equal(t, tc.httpResponse, msg, tc.name)
		}
	}
}

func TestNewWallet(t *testing.T) {
	w, err := deterministic.NewWallet("filename", "test", "seed", wallet.OptionGenerateN(1))
	require.NoError(t, err)
	es, err := w.GetEntries()
	require.NoError(t, err)
	fmt.Println(es[0].Address.String())
}

func TestGetWallets(t *testing.T) {
	var pubkeys []cipher.PubKey
	var seckeys []cipher.SecKey
	var addrs []cipher.Address

	seckeys, err := cipher.GenerateDeterministicKeyPairs([]byte("seed"), 5)
	require.NoError(t, err)
	for _, sk := range seckeys {
		pk := cipher.MustPubKeyFromSecKey(sk)
		pubkeys = append(pubkeys, pk)
		addrs = append(addrs, cipher.AddressFromPubKey(pk))
	}

	// create a deterministic and overwrite its meta data
	makeDeterministicWalletWithMeta := func(n uint64, m wallet.Meta) wallet.Wallet {
		w, err := deterministic.NewWallet(
			"wallet",
			"filename",
			"seed",
			wallet.OptionGenerateN(n))
		require.NoError(t, err)
		// overwrite
		for k, v := range m {
			w.Meta[k] = v
		}
		return w
	}

	cases := []struct {
		name               string
		method             string
		status             int
		err                string
		getWalletsResponse wallet.Wallets
		getWalletsErr      error
		httpResponse       []*WalletResponse
	}{
		{
			name:   "405",
			method: http.MethodPost,
			status: http.StatusMethodNotAllowed,
			err:    "405 Method Not Allowed",
		},
		{
			name:          "403 - wallet API disabled",
			method:        http.MethodGet,
			status:        http.StatusForbidden,
			err:           "403 Forbidden",
			getWalletsErr: wallet.ErrWalletAPIDisabled,
		},
		{
			name:               "200 no wallets",
			method:             http.MethodGet,
			status:             http.StatusOK,
			getWalletsResponse: nil,
			httpResponse:       []*WalletResponse{},
		},
		{
			name:               "200 no wallets 2",
			method:             http.MethodGet,
			status:             http.StatusOK,
			getWalletsResponse: wallet.Wallets{},
			httpResponse:       []*WalletResponse{},
		},
		{
			name:   "200",
			method: http.MethodGet,
			status: http.StatusOK,
			getWalletsResponse: wallet.Wallets{
				"foofilename": makeDeterministicWalletWithMeta(1, wallet.Meta{
					"foo":        "bar",
					"seed":       "fooseed",
					"lastSeed":   "foolastseed",
					"coin":       "foocoin",
					"filename":   "foofilename",
					"label":      "foolabel",
					"type":       "footype",
					"version":    "fooversion",
					"cryptoType": "foocryptotype",
					"tm":         "345678",
					"encrypted":  "true",
				}),
				"foofilename2": makeDeterministicWalletWithMeta(2,
					wallet.Meta{
						"foo":        "bar2",
						"seed":       "fooseed2",
						"lastSeed":   "foolastseed2",
						"coin":       "foocoin",
						"filename":   "foofilename2",
						"label":      "foolabel2",
						"type":       "footype",
						"version":    "fooversion",
						"cryptoType": "foocryptotype",
						"tm":         "123456",
						"encrypted":  "false",
					}),
				"foofilename3": makeDeterministicWalletWithMeta(3,
					wallet.Meta{
						"foo":        "bar3",
						"seed":       "fooseed3",
						"lastSeed":   "foolastseed3",
						"coin":       "foocoin",
						"filename":   "foofilename3",
						"label":      "foolabel3",
						"type":       "footype",
						"version":    "fooversion",
						"cryptoType": "foocryptotype",
						"tm":         "234567",
						"encrypted":  "true",
					}),
			},
			httpResponse: []*WalletResponse{
				{
					Meta: readable.WalletMeta{
						Coin:       "foocoin",
						Filename:   "foofilename2",
						Label:      "foolabel2",
						Type:       "footype",
						Version:    "fooversion",
						CryptoType: "foocryptotype",
						Timestamp:  123456,
						Encrypted:  false,
					},
					Entries: []readable.WalletEntry{
						{
							Address: addrs[0].String(),
							Public:  pubkeys[0].Hex(),
						},
						{
							Address: addrs[1].String(),
							Public:  pubkeys[1].Hex(),
						},
					},
				},
				{
					Meta: readable.WalletMeta{
						Coin:       "foocoin",
						Filename:   "foofilename3",
						Label:      "foolabel3",
						Type:       "footype",
						Version:    "fooversion",
						CryptoType: "foocryptotype",
						Timestamp:  234567,
						Encrypted:  true,
					},
					Entries: []readable.WalletEntry{
						{
							Address: addrs[0].String(),
							Public:  pubkeys[0].Hex(),
						},
						{
							Address: addrs[1].String(),
							Public:  pubkeys[1].Hex(),
						},
						{
							Address: addrs[2].String(),
							Public:  pubkeys[2].Hex(),
						},
					},
				},
				{
					Meta: readable.WalletMeta{
						Coin:       "foocoin",
						Filename:   "foofilename",
						Label:      "foolabel",
						Type:       "footype",
						Version:    "fooversion",
						CryptoType: "foocryptotype",
						Timestamp:  345678,
						Encrypted:  true,
					},
					Entries: []readable.WalletEntry{
						{
							Address: addrs[0].String(),
							Public:  pubkeys[0].Hex(),
						},
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}
			gateway.On("GetWallets").Return(tc.getWalletsResponse, tc.getWalletsErr)

			endpoint := "/api/v1/wallets"

			req, err := http.NewRequest(tc.method, endpoint, nil)
			require.NoError(t, err)

			setCSRFParameters(t, tokenValid, req)

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = false

			handler := newServerMux(cfg, gateway)

			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "got `%v` want `%v`",
				tc.name, status, tc.status)

			if status != http.StatusOK {
				require.Equal(t, tc.err, strings.TrimSpace(rr.Body.String()), "got `%v`| %d, want `%v`",
					strings.TrimSpace(rr.Body.String()), status, tc.err)
			} else {
				var msg []*WalletResponse
				err := json.Unmarshal(rr.Body.Bytes(), &msg)
				require.NoError(t, err)
				require.NotNil(t, msg)
				require.Equal(t, tc.httpResponse, msg, tc.name)
			}

		})
	}
}

func TestWalletUnloadHandler(t *testing.T) {
	tt := []struct {
		name            string
		method          string
		status          int
		err             string
		walletID        string
		unloadWalletErr error
		csrfDisabled    bool
	}{
		{
			name:     "405",
			method:   http.MethodGet,
			status:   http.StatusMethodNotAllowed,
			err:      "405 Method Not Allowed",
			walletID: "wallet.wlt",
		},
		{
			name:   "400 - missing wallet id",
			method: http.MethodPost,
			status: http.StatusBadRequest,
			err:    "400 Bad Request - missing wallet id",
		},
		{
			name:            "403 - Forbidden - wallet API disabled",
			method:          http.MethodPost,
			status:          http.StatusForbidden,
			err:             "403 Forbidden",
			walletID:        "wallet.wlt",
			unloadWalletErr: wallet.ErrWalletAPIDisabled,
		},
		{
			name:     "200 - ok",
			method:   http.MethodPost,
			status:   http.StatusOK,
			walletID: "wallet.wlt",
		},
		{
			name:         "200 - ok, csrf disabled",
			method:       http.MethodPost,
			status:       http.StatusOK,
			walletID:     "wallet.wlt",
			csrfDisabled: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}
			gateway.On("UnloadWallet", tc.walletID).Return(tc.unloadWalletErr)

			endpoint := "/api/v1/wallet/unload"
			v := url.Values{}
			v.Add("id", tc.walletID)

			req, err := http.NewRequest(tc.method, endpoint, strings.NewReader(v.Encode()))
			require.NoError(t, err)
			req.Header.Add("Content-Type", ContentTypeForm)

			if tc.csrfDisabled {
				setCSRFParameters(t, tokenInvalid, req)
			} else {
				setCSRFParameters(t, tokenValid, req)
			}

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = tc.csrfDisabled

			handler := newServerMux(cfg, gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "wrong status code: got `%v` want `%v`", status, tc.status)

			if status != http.StatusOK {
				require.Equal(t, tc.err, strings.TrimSpace(rr.Body.String()), "got `%v`| %d, want `%v`",
					strings.TrimSpace(rr.Body.String()), status, tc.err)
			}
		})
	}
}

func TestEncryptWallet(t *testing.T) {
	_, responseEntries := makeEntries([]byte("seed"), 5)
	type gatewayReturnPair struct {
		w   wallet.Wallet
		err error
	}

	tt := []struct {
		name          string
		method        string
		wltID         string
		password      string
		gatewayReturn gatewayReturnPair
		status        int
		expectWallet  WalletResponse
		expectErr     string
	}{
		{
			name:     "200 - OK",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturn: gatewayReturnPair{
				w: func() wallet.Wallet {
					wlt, err := deterministic.NewWallet(
						"wallet.wlt",
						"test",
						"seed",
						wallet.OptionPassword([]byte("pwd")),
						wallet.OptionGenerateN(5),
						wallet.OptionEncrypt(true))
					require.NoError(t, err)
					wlt.SetTimestamp(0)
					return wlt
				}(),
			},
			status: http.StatusOK,
			expectWallet: WalletResponse{
				Meta: readable.WalletMeta{
					Coin:       "skycoin",
					Filename:   "wallet.wlt",
					Label:      "test",
					Type:       "deterministic",
					Version:    "0.4",
					CryptoType: "scrypt-chacha20poly1305",
					Encrypted:  true,
				},
				Entries: responseEntries,
			},
		},
		{
			name:     "403 Forbidden",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrWalletAPIDisabled,
			},
			status:    http.StatusForbidden,
			expectErr: "403 Forbidden",
		},
		{
			name:      "405 Method Not Allowed",
			method:    http.MethodGet,
			wltID:     "wallet.wlt",
			password:  "pwd",
			status:    http.StatusMethodNotAllowed,
			expectErr: "405 Method Not Allowed",
		},
		{
			name:     "400 - Missing Password",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "",
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrMissingPassword,
			},
			status:    http.StatusBadRequest,
			expectErr: "400 Bad Request - missing password",
		},
		{
			name:      "400 - Missing Wallet Id",
			method:    http.MethodPost,
			wltID:     "",
			status:    http.StatusBadRequest,
			expectErr: "400 Bad Request - missing wallet id",
		},
		{
			name:     "400 Bad Request - Invalid Password",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrInvalidPassword,
			},
			status:    http.StatusBadRequest,
			expectErr: "400 Bad Request - invalid password",
		},
		{
			name:     "404 - Wallet Not Found",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrWalletNotExist,
			},
			status:    http.StatusNotFound,
			expectErr: "404 Not Found",
		},
		{
			name:     "400 - Wallet Is Encrypted",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrWalletEncrypted,
			},
			status:    http.StatusBadRequest,
			expectErr: "400 Bad Request - wallet is encrypted",
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}
			gateway.On("EncryptWallet", tc.wltID, []byte(tc.password)).Return(tc.gatewayReturn.w, tc.gatewayReturn.err)

			endpoint := "/api/v1/wallet/encrypt"
			v := url.Values{}
			v.Add("id", tc.wltID)
			v.Add("password", tc.password)

			req, err := http.NewRequest(tc.method, endpoint, strings.NewReader(v.Encode()))
			require.NoError(t, err)
			req.Header.Add("Content-Type", ContentTypeForm)

			setCSRFParameters(t, tokenValid, req)

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = false

			handler := newServerMux(cfg, gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "wrong status code: got `%v` want `%v`, body: %v", status, tc.status, rr.Body.String())

			if status != http.StatusOK {
				require.Equal(t, tc.expectErr, strings.TrimSpace(rr.Body.String()))
				return
			}

			var rsp WalletResponse
			err = json.Unmarshal(rr.Body.Bytes(), &rsp)
			require.NoError(t, err)
			require.Equal(t, tc.expectWallet, rsp)
		})
	}
}

func TestDecryptWallet(t *testing.T) {
	_, responseEntries := makeEntries([]byte("seed"), 5)
	type gatewayReturnPair struct {
		w   wallet.Wallet
		err error
	}

	makeDeterministicWallet := func() wallet.Wallet {
		w, err := deterministic.NewWallet(
			"wallet",
			"filename",
			"seed",
			wallet.OptionGenerateN(5))
		require.NoError(t, err)
		w.SetTimestamp(0)
		return w
	}

	tt := []struct {
		name          string
		method        string
		wltID         string
		password      string
		gatewayReturn gatewayReturnPair
		status        int
		expectWallet  WalletResponse
		expectErr     string
		csrfDisabled  bool
	}{
		{
			name:     "200 OK",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturn: gatewayReturnPair{
				w: makeDeterministicWallet(),
			},
			status: http.StatusOK,
			expectWallet: WalletResponse{
				Meta: readable.WalletMeta{
					Coin:       "skycoin",
					Filename:   "wallet",
					Label:      "filename",
					Type:       "deterministic",
					Version:    "0.4",
					CryptoType: "scrypt-chacha20poly1305",
					Encrypted:  false,
				},
				Entries: responseEntries,
			},
		},
		{
			name:     "200 OK CSRF disabled",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturn: gatewayReturnPair{
				w: makeDeterministicWallet(),
			},
			status: http.StatusOK,
			expectWallet: WalletResponse{
				Meta: readable.WalletMeta{
					Coin:       "skycoin",
					Filename:   "wallet",
					Label:      "filename",
					Type:       "deterministic",
					Version:    "0.4",
					CryptoType: "scrypt-chacha20poly1305",
					Encrypted:  false,
				},
				Entries: responseEntries,
			},
			csrfDisabled: true,
		},

		{
			name:     "403 Forbidden",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrWalletAPIDisabled,
			},
			status:    http.StatusForbidden,
			expectErr: "403 Forbidden",
		},
		{
			name:      "405 Method Not Allowed",
			method:    http.MethodGet,
			status:    http.StatusMethodNotAllowed,
			expectErr: "405 Method Not Allowed",
		},
		{
			name:      "400 - Missing Wallet ID",
			method:    http.MethodPost,
			wltID:     "",
			password:  "",
			status:    http.StatusBadRequest,
			expectErr: "400 Bad Request - missing wallet id",
		},
		{
			name:     "400 - Missing Password",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "",
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrMissingPassword,
			},
			status:    http.StatusBadRequest,
			expectErr: "400 Bad Request - missing password",
		},
		{
			name:     "400 - Wallet IS Not Encrypted",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrWalletNotEncrypted,
			},
			status:    http.StatusBadRequest,
			expectErr: "400 Bad Request - wallet is not encrypted",
		},
		{
			name:     "400 Bad Request - Invalid Password",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrInvalidPassword,
			},
			status:    http.StatusBadRequest,
			expectErr: "400 Bad Request - invalid password",
		},
		{
			name:     "404 - Wallet Does Not Exist",
			method:   http.MethodPost,
			wltID:    "wallet.wlt",
			password: "pwd",
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrWalletNotExist,
			},
			status:    http.StatusNotFound,
			expectErr: "404 Not Found",
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}
			gateway.On("DecryptWallet", tc.wltID, []byte(tc.password)).Return(tc.gatewayReturn.w, tc.gatewayReturn.err)

			endpoint := "/api/v1/wallet/decrypt"
			v := url.Values{}
			v.Add("id", tc.wltID)
			v.Add("password", tc.password)

			req, err := http.NewRequest(tc.method, endpoint, strings.NewReader(v.Encode()))
			require.NoError(t, err)
			req.Header.Add("Content-Type", ContentTypeForm)

			setCSRFParameters(t, tokenValid, req)

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = false

			handler := newServerMux(cfg, gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "wrong status code: got `%v` want `%v`", status, tc.status)

			if status != http.StatusOK {
				require.Equal(t, tc.expectErr, strings.TrimSpace(rr.Body.String()))
				return
			}

			var rsp WalletResponse
			err = json.Unmarshal(rr.Body.Bytes(), &rsp)
			require.NoError(t, err)
			require.Equal(t, tc.expectWallet, rsp)
		})
	}
}

// makeEntries derives N wallet address entries from given seed
// Returns set of entry.Entry and wallet.ReadableEntry, the readable
// entries' secrets are removed.
func makeEntries(seed []byte, n int) ([]wallet.Entry, []readable.WalletEntry) { //nolint:unparam
	seckeys := cipher.MustGenerateDeterministicKeyPairs(seed, n)
	var entries []wallet.Entry
	var responseEntries []readable.WalletEntry
	for i, seckey := range seckeys {
		pubkey := cipher.MustPubKeyFromSecKey(seckey)
		entries = append(entries, wallet.Entry{
			Address: cipher.AddressFromPubKey(pubkey),
			Public:  pubkey,
			Secret:  seckey,
		})
		responseEntries = append(responseEntries, readable.WalletEntry{
			Address: entries[i].Address.String(),
			Public:  entries[i].Public.Hex(),
		})
	}
	return entries, responseEntries
}

type mockTxnsFinder struct {
}

func (tf *mockTxnsFinder) AddressesActivity(addrs []cipher.Addresser) ([]bool, error) {
	return nil, nil
}

func TestWalletRecover(t *testing.T) {
	type gatewayReturnPair struct {
		w   wallet.Wallet
		err error
	}

	okWalletUnencrypted, err := wallet.NewWallet(
		"foo",
		"foolabel",
		"fooseed",
		wallet.Options{
			Type:      wallet.WalletTypeDeterministic,
			Coin:      wallet.CoinTypeSkycoin,
			GenerateN: 10,
			TF:        &mockTxnsFinder{},
		})
	require.NoError(t, err)
	okWalletUnencryptedResponse, err := NewWalletResponse(okWalletUnencrypted)
	require.NoError(t, err)

	okWalletEncrypted, err := wallet.NewWallet(
		"foo",
		"foolabel",
		"fooseed",
		wallet.Options{
			Type:       wallet.WalletTypeDeterministic,
			Coin:       wallet.CoinTypeSkycoin,
			Encrypt:    true,
			Password:   []byte("foopassword"),
			CryptoType: crypto.CryptoTypeScryptChacha20poly1305Insecure,
			GenerateN:  10,
			TF:         &mockTxnsFinder{},
		})
	require.NoError(t, err)
	okWalletEncryptedResponse, err := NewWalletResponse(okWalletEncrypted)
	require.NoError(t, err)

	cases := []struct {
		name          string
		method        string
		status        int
		contentType   string
		req           *WalletRecoverRequest
		httpBody      string
		httpResponse  HTTPResponse
		gatewayReturn gatewayReturnPair
	}{
		{
			name:         "method not allowed",
			method:       http.MethodGet,
			status:       http.StatusMethodNotAllowed,
			contentType:  ContentTypeJSON,
			httpBody:     toJSON(t, WalletRecoverRequest{}),
			httpResponse: NewHTTPErrorResponse(http.StatusMethodNotAllowed, "Method Not Allowed"),
		},
		{
			name:         "wrong content-type",
			method:       http.MethodPost,
			status:       http.StatusUnsupportedMediaType,
			contentType:  ContentTypeForm,
			httpBody:     toJSON(t, WalletRecoverRequest{}),
			httpResponse: NewHTTPErrorResponse(http.StatusUnsupportedMediaType, "Unsupported Media Type"),
		},
		{
			name:         "empty json body",
			method:       http.MethodPost,
			status:       http.StatusBadRequest,
			contentType:  ContentTypeJSON,
			httpBody:     "",
			httpResponse: NewHTTPErrorResponse(http.StatusBadRequest, "EOF"),
		},
		{
			name:        "id missing",
			method:      http.MethodPost,
			status:      http.StatusBadRequest,
			contentType: ContentTypeJSON,
			req: &WalletRecoverRequest{
				Seed: "fooseed",
			},
			httpResponse: NewHTTPErrorResponse(http.StatusBadRequest, "id is required"),
		},
		{
			name:        "seed missing",
			method:      http.MethodPost,
			status:      http.StatusBadRequest,
			contentType: ContentTypeJSON,
			req: &WalletRecoverRequest{
				ID: "foo",
			},
			httpResponse: NewHTTPErrorResponse(http.StatusBadRequest, "seed is required"),
		},
		{
			name:        "wallet not encrypted",
			method:      http.MethodPost,
			status:      http.StatusBadRequest,
			contentType: ContentTypeJSON,
			req: &WalletRecoverRequest{
				ID:   "foo",
				Seed: "fooseed",
			},
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrWalletNotEncrypted,
			},
			httpResponse: NewHTTPErrorResponse(http.StatusBadRequest, wallet.ErrWalletNotEncrypted.Error()),
		},
		{
			name:        "wallet seed wrong",
			method:      http.MethodPost,
			status:      http.StatusBadRequest,
			contentType: ContentTypeJSON,
			req: &WalletRecoverRequest{
				ID:   "foo",
				Seed: "fooseed",
			},
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrWalletRecoverSeedWrong,
			},
			httpResponse: NewHTTPErrorResponse(http.StatusBadRequest, wallet.ErrWalletRecoverSeedWrong.Error()),
		},
		{
			name:        "wallet does not exist",
			method:      http.MethodPost,
			status:      http.StatusNotFound,
			contentType: ContentTypeJSON,
			req: &WalletRecoverRequest{
				ID:   "foo",
				Seed: "fooseed",
			},
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrWalletNotExist,
			},
			httpResponse: NewHTTPErrorResponse(http.StatusNotFound, "Not Found"),
		},
		{
			name:        "wallet api disabled",
			method:      http.MethodPost,
			status:      http.StatusForbidden,
			contentType: ContentTypeJSON,
			req: &WalletRecoverRequest{
				ID:   "foo",
				Seed: "fooseed",
			},
			gatewayReturn: gatewayReturnPair{
				err: wallet.ErrWalletAPIDisabled,
			},
			httpResponse: NewHTTPErrorResponse(http.StatusForbidden, ""),
		},
		{
			name:        "wallet other error",
			method:      http.MethodPost,
			status:      http.StatusInternalServerError,
			contentType: ContentTypeJSON,
			req: &WalletRecoverRequest{
				ID:   "foo",
				Seed: "fooseed",
			},
			gatewayReturn: gatewayReturnPair{
				err: errors.New("wallet error"),
			},
			httpResponse: NewHTTPErrorResponse(http.StatusInternalServerError, "wallet error"),
		},
		{
			name:        "ok, no password",
			method:      http.MethodPost,
			status:      http.StatusOK,
			contentType: ContentTypeJSON,
			req: &WalletRecoverRequest{
				ID:   "foo",
				Seed: "fooseed",
			},
			gatewayReturn: gatewayReturnPair{
				w: okWalletUnencrypted,
			},
			httpResponse: HTTPResponse{
				Data: *okWalletUnencryptedResponse,
			},
		},
		{
			name:        "ok, seed passphrase, no password",
			method:      http.MethodPost,
			status:      http.StatusOK,
			contentType: ContentTypeJSON,
			req: &WalletRecoverRequest{
				ID:             "foo",
				Seed:           "fooseed",
				SeedPassphrase: "fooseedpassphrase",
			},
			gatewayReturn: gatewayReturnPair{
				w: okWalletUnencrypted,
			},
			httpResponse: HTTPResponse{
				Data: *okWalletUnencryptedResponse,
			},
		},
		{
			name:        "ok, password",
			method:      http.MethodPost,
			status:      http.StatusOK,
			contentType: ContentTypeJSON,
			req: &WalletRecoverRequest{
				ID:       "foo",
				Seed:     "fooseed",
				Password: "foopassword",
			},
			gatewayReturn: gatewayReturnPair{
				w: okWalletEncrypted,
			},
			httpResponse: HTTPResponse{
				Data: *okWalletEncryptedResponse,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &MockGatewayer{}
			if tc.req != nil {
				var password []byte
				if tc.req.Password != "" {
					password = []byte(tc.req.Password)
				}
				gateway.On("RecoverWallet", tc.req.ID, tc.req.Seed, tc.req.SeedPassphrase, password).Return(tc.gatewayReturn.w, tc.gatewayReturn.err)
			}

			if tc.httpBody == "" && tc.req != nil {
				tc.httpBody = toJSON(t, tc.req)
			}

			endpoint := "/api/v2/wallet/recover"
			req, err := http.NewRequest(tc.method, endpoint, strings.NewReader(tc.httpBody))
			require.NoError(t, err)

			contentType := tc.contentType
			if contentType == "" {
				contentType = ContentTypeJSON
			}

			req.Header.Set("Content-Type", contentType)

			setCSRFParameters(t, tokenValid, req)

			rr := httptest.NewRecorder()

			cfg := defaultMuxConfig()
			cfg.disableCSRF = false

			handler := newServerMux(cfg, gateway)
			handler.ServeHTTP(rr, req)

			status := rr.Code
			require.Equal(t, tc.status, status, "got `%v` want `%v`", status, tc.status)

			var rsp ReceivedHTTPResponse
			err = json.Unmarshal(rr.Body.Bytes(), &rsp)
			require.NoError(t, err)

			require.Equal(t, tc.httpResponse.Error, rsp.Error)

			if rsp.Data == nil {
				require.Nil(t, tc.httpResponse.Data)
			} else {
				require.NotNil(t, tc.httpResponse.Data)

				var wltRsp WalletResponse
				err := json.Unmarshal(rsp.Data, &wltRsp)
				require.NoError(t, err)

				require.Equal(t, tc.httpResponse.Data.(WalletResponse), wltRsp)
			}
		})
	}
}
