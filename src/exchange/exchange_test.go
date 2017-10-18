package exchange

import (
	"errors"
	"sync"
	"testing"

	"time"

	"github.com/boltdb/bolt"
	"github.com/sirupsen/logrus"
	logrus_test "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/skycoin/teller/src/scanner"
	"github.com/skycoin/teller/src/sender"
	"github.com/skycoin/teller/src/util/testutil"
)

type dummySender struct {
	sync.RWMutex
	txids          []string
	sendErr        error
	confirmErr     error
	txidConfirmMap map[string]bool
}

func newDummySender() *dummySender {
	return &dummySender{
		txidConfirmMap: make(map[string]bool),
	}
}

func (send *dummySender) Send(destAddr string, coins uint64) *sender.SendResponse {
	req := sender.SendRequest{
		Coins:   coins,
		Address: destAddr,
		RspC:    make(chan *sender.SendResponse, 1),
	}

	if send.sendErr != nil {
		return &sender.SendResponse{
			Err: send.sendErr,
			Req: req,
		}
	}

	txid := send.nextTxid()

	return &sender.SendResponse{
		Txid: txid,
		Req:  req,
	}
}

func (send *dummySender) IsTxConfirmed(txid string) *sender.ConfirmResponse {
	send.RLock()
	defer send.RUnlock()

	req := sender.ConfirmRequest{
		Txid: txid,
	}

	if send.confirmErr != nil {
		return &sender.ConfirmResponse{
			Err: send.confirmErr,
			Req: req,
		}
	}

	confirmed := send.txidConfirmMap[txid]
	return &sender.ConfirmResponse{
		Confirmed: confirmed,
		Req:       req,
	}
}

func (send *dummySender) nextTxid() string {
	send.Lock()
	defer send.Unlock()

	if len(send.txids) == 0 {
		panic("need more txids added to dummySender")
	}

	txid := send.txids[0]
	send.txids = send.txids[1:]

	return txid
}

func (send *dummySender) addTxidResponse(txid string) {
	send.Lock()
	defer send.Unlock()

	send.txids = append(send.txids, txid)
}

func (send *dummySender) setTxConfirmed(txid string) {
	send.Lock()
	defer send.Unlock()

	send.txidConfirmMap[txid] = true
}

type dummyScanner struct {
	dvC   chan scanner.DepositNote
	addrs []string
}

func newDummyScanner() *dummyScanner {
	return &dummyScanner{
		dvC: make(chan scanner.DepositNote, 10),
	}
}

func (scan *dummyScanner) AddScanAddress(addr string) error {
	scan.addrs = append(scan.addrs, addr)
	return nil
}

func (scan *dummyScanner) GetDeposit() <-chan scanner.DepositNote {
	return scan.dvC
}

func (scan *dummyScanner) GetScanAddresses() ([]string, error) {
	return []string{}, nil
}

func (scan *dummyScanner) addDeposit(d scanner.DepositNote) {
	scan.dvC <- d
}

func (scan *dummyScanner) stop() {
	close(scan.dvC)
}

const (
	testSkyBtcRate  int64 = 100
	dbScanTimeout         = time.Second * 3
	dbCheckWaitTime       = time.Millisecond * 300
)

func newTestExchange(t *testing.T, log *logrus.Logger, db *bolt.DB) *Exchange {
	store, err := NewStore(log, db)
	require.NoError(t, err)

	e, err := NewExchange(log, store, newDummyScanner(), newDummySender(), Config{
		Rate: testSkyBtcRate,
		TxConfirmationCheckWait: time.Millisecond * 100,
	})
	require.NoError(t, err)
	return e
}

func setupExchange(t *testing.T, log *logrus.Logger) (*Exchange, func(), func()) {
	db, shutdownDB := testutil.PrepareDB(t)

	e := newTestExchange(t, log, db)

	done := make(chan struct{})
	run := func() {
		err := e.Run()
		require.NoError(t, err)
		close(done)
	}

	shutdown := func() {
		shutdownDB()
		<-done
	}

	return e, run, shutdown
}

func runExchange(t *testing.T) (*Exchange, func(), *logrus_test.Hook) {
	log, hook := testutil.NewLogger(t)
	e, run, shutdown := setupExchange(t, log)
	go run()
	return e, shutdown, hook
}

func runExchangeMockStore(t *testing.T) (*Exchange, func(), *logrus_test.Hook) {
	store := &MockStore{}
	log, hook := testutil.NewLogger(t)

	e, err := NewExchange(log, store, newDummyScanner(), newDummySender(), Config{
		Rate: testSkyBtcRate,
		TxConfirmationCheckWait: time.Millisecond * 100,
	})
	require.NoError(t, err)

	done := make(chan struct{})
	run := func() {
		err := e.Run()
		require.NoError(t, err)
		close(done)
	}
	go run()

	shutdown := func() {
		<-done
	}

	return e, shutdown, hook
}

func TestRunShutdown(t *testing.T) {
	// Tests a simple start and stop, with no scanner activity
	e, shutdown, _ := runExchange(t)
	defer shutdown()
	defer e.Shutdown()
}

func TestRunScannerClosed(t *testing.T) {
	// Tests that there is no problem when the scanner closes
	e, shutdown, _ := runExchange(t)
	defer shutdown()
	defer e.Shutdown()
	e.scanner.(*dummyScanner).stop()
}

func TestRunSend(t *testing.T) {
	e, shutdown, _ := runExchange(t)
	defer shutdown()
	defer e.Shutdown()

	skyAddr := "foo-sky-addr"
	btcAddr := "foo-btc-addr"
	err := e.store.BindAddress(skyAddr, btcAddr)
	require.NoError(t, err)

	txid := "foo-sky-txid"
	e.sender.(*dummySender).addTxidResponse(txid)

	dn := scanner.DepositNote{
		Deposit: scanner.Deposit{
			Address: btcAddr,
			Value:   1e8,
			Height:  20,
			Tx:      "foo-tx",
			N:       2,
		},
		ErrC: make(chan error, 1),
	}
	e.scanner.(*dummyScanner).addDeposit(dn)

	// First loop calls saveIncomingDeposit
	// nil is written to ErrC after this method finishes
	err = <-dn.ErrC
	require.NoError(t, err)

	// Second loop calls processWaitSendDeposit
	// It sends the coins, then confirms them

	// Periodically check the database until we observe the sent deposit
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-time.After(dbCheckWaitTime):
				di, err := e.store.GetDepositInfo(dn.Deposit.TxN())
				require.NoError(t, err)
				if di.Status != StatusWaitConfirm {
					continue
				}

				require.NotEmpty(t, di.UpdatedAt)

				expectedDeposit := DepositInfo{
					Seq:        1,
					UpdatedAt:  di.UpdatedAt,
					Status:     StatusWaitConfirm,
					SkyAddress: skyAddr,
					BtcAddress: dn.Deposit.Address,
					BtcTx:      dn.Deposit.TxN(),
					Txid:       txid,
					SkySent:    100e6,
					SkyBtcRate: testSkyBtcRate,
					Deposit:    dn.Deposit,
				}

				require.Equal(t, expectedDeposit, di)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(dbScanTimeout):
		t.Fatal("Waiting for sent deposit timed out")
	}

	// Mark the deposit as confirmed
	e.sender.(*dummySender).setTxConfirmed(txid)

	// Periodically check the database until we observe the confirmed deposit
	done = make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-time.After(dbCheckWaitTime):
				di, err := e.store.GetDepositInfo(dn.Deposit.TxN())
				require.NoError(t, err)
				if di.Status != StatusDone {
					continue
				}

				require.NotEmpty(t, di.UpdatedAt)

				expectedDeposit := DepositInfo{
					Seq:        1,
					UpdatedAt:  di.UpdatedAt,
					Status:     StatusDone,
					SkyAddress: skyAddr,
					BtcAddress: dn.Deposit.Address,
					BtcTx:      dn.Deposit.TxN(),
					Txid:       txid,
					SkySent:    100e6,
					SkyBtcRate: testSkyBtcRate,
					Deposit:    dn.Deposit,
				}

				require.Equal(t, expectedDeposit, di)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(dbScanTimeout):
		t.Fatal("Waiting for confirmed deposit timed out")
	}
}

func TestSendFailure(t *testing.T) {
	// Test that we save the rate when first creating, not on send
	// To do this, force the sender to return errors to block the transition to
	// StatusWaitConfirm
	e, shutdown, _ := runExchange(t)
	defer shutdown()
	defer e.Shutdown()

	skyAddr := "foo-sky-addr"
	btcAddr := "foo-btc-addr"
	err := e.store.BindAddress(skyAddr, btcAddr)
	require.NoError(t, err)

	// Force sender to return a send error so that the deposit stays at StatusWaitSend
	e.sender.(*dummySender).sendErr = errors.New("fake send error")

	dn := scanner.DepositNote{
		Deposit: scanner.Deposit{
			Address: btcAddr,
			Value:   1e8,
			Height:  20,
			Tx:      "foo-tx",
			N:       2,
		},
		ErrC: make(chan error, 1),
	}
	e.scanner.(*dummyScanner).addDeposit(dn)

	// First loop calls saveIncomingDeposit
	// nil is written to ErrC after this method finishes
	err = <-dn.ErrC
	require.NoError(t, err)

	// Check the DepositInfo in the database
	di, err := e.store.GetDepositInfo(dn.Deposit.TxN())
	require.NoError(t, err)
	require.NotEmpty(t, di.UpdatedAt)
	require.Equal(t, DepositInfo{
		Seq:        1,
		UpdatedAt:  di.UpdatedAt,
		SkyAddress: skyAddr,
		BtcAddress: btcAddr,
		BtcTx:      dn.Deposit.TxN(),
		Status:     StatusWaitSend,
		SkyBtcRate: testSkyBtcRate,
		Deposit:    dn.Deposit,
	}, di)
}

func TestTxConfirmFailure(t *testing.T) {
	e, shutdown, _ := runExchange(t)
	defer shutdown()
	defer e.Shutdown()

	skyAddr := "foo-sky-addr"
	btcAddr := "foo-btc-addr"
	err := e.store.BindAddress(skyAddr, btcAddr)
	require.NoError(t, err)

	txid := "foo-sky-txid"
	e.sender.(*dummySender).addTxidResponse(txid)

	// Force sender to return a confirm error so that the deposit stays at StatusWaitConfirm
	e.sender.(*dummySender).confirmErr = errors.New("fake confirm error")

	dn := scanner.DepositNote{
		Deposit: scanner.Deposit{
			Address: btcAddr,
			Value:   1e8,
			Height:  20,
			Tx:      "foo-tx",
			N:       2,
		},
		ErrC: make(chan error, 1),
	}
	e.scanner.(*dummyScanner).addDeposit(dn)

	// First loop calls saveIncomingDeposit
	// nil is written to ErrC after this method finishes
	err = <-dn.ErrC
	require.NoError(t, err)

	// Wait for StatusWaitSend deposit in the database
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-time.After(dbCheckWaitTime):
				// Check the DepositInfo in the database
				di, err := e.store.GetDepositInfo(dn.Deposit.TxN())
				require.NoError(t, err)
				require.NotEmpty(t, di.UpdatedAt)
				require.Equal(t, DepositInfo{
					Seq:        1,
					UpdatedAt:  di.UpdatedAt,
					SkyAddress: skyAddr,
					BtcAddress: btcAddr,
					BtcTx:      dn.Deposit.TxN(),
					Txid:       txid,
					SkySent:    100e6,
					Status:     StatusWaitConfirm,
					SkyBtcRate: testSkyBtcRate,
					Deposit:    dn.Deposit,
				}, di)

				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(dbScanTimeout):
		t.Fatal("Waiting to check for StatusWaitSend deposits timed out")
	}
}

func TestQuitBeforeConfirm(t *testing.T) {
	e, shutdown, _ := runExchange(t)
	defer shutdown()

	skyAddr := "foo-sky-addr"
	btcAddr := "foo-btc-addr"
	err := e.store.BindAddress(skyAddr, btcAddr)
	require.NoError(t, err)

	txid := "foo-sky-txid"
	e.sender.(*dummySender).addTxidResponse(txid)

	dn := scanner.DepositNote{
		Deposit: scanner.Deposit{
			Address: btcAddr,
			Value:   1e8,
			Height:  20,
			Tx:      "foo-tx",
			N:       2,
		},
		ErrC: make(chan error, 1),
	}
	e.scanner.(*dummyScanner).addDeposit(dn)

	// First loop calls saveIncomingDeposit
	// nil is written to ErrC after this method finishes
	err = <-dn.ErrC
	require.NoError(t, err)

	// Second loop calls processWaitSendDeposit
	// It sends the coins, then confirms them

	expectedDeposit := DepositInfo{
		Seq:        1,
		Status:     StatusWaitConfirm,
		SkyAddress: skyAddr,
		BtcAddress: dn.Deposit.Address,
		BtcTx:      dn.Deposit.TxN(),
		Txid:       txid,
		SkySent:    100e6,
		SkyBtcRate: testSkyBtcRate,
		Deposit:    dn.Deposit,
	}

	// Periodically check the database until we observe the sent deposit
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-time.After(dbCheckWaitTime):
				di, err := e.store.GetDepositInfo(dn.Deposit.TxN())
				require.NoError(t, err)
				if di.Status != StatusWaitConfirm {
					continue
				}

				require.NotEmpty(t, di.UpdatedAt)

				ed := expectedDeposit
				ed.UpdatedAt = di.UpdatedAt

				require.Equal(t, ed, di)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(dbScanTimeout):
		t.Fatal("Waiting for sent deposit timed out")
	}

	e.Shutdown()

	di, err := e.store.GetDepositInfo(dn.Deposit.TxN())
	require.NoError(t, err)

	require.NotEmpty(t, di.UpdatedAt)
	ed := expectedDeposit
	ed.UpdatedAt = di.UpdatedAt

	require.Equal(t, ed, di)
}

func testRunProcessDepositBacklog(t *testing.T, dis []DepositInfo, configureSender func(*Exchange, DepositInfo)) {
	log, _ := testutil.NewLogger(t)
	e, run, shutdown := setupExchange(t, log)

	for _, di := range dis {
		configureSender(e, di)

		err := e.store.AddDepositInfo(di)
		require.NoError(t, err)
	}

	filter := func(di DepositInfo) bool {
		return di.Status == StatusDone
	}

	// Make sure that there are no confirmed deposits yet
	confirmed, err := e.store.GetDepositInfoArray(filter)
	require.NoError(t, err)
	require.Len(t, confirmed, 0)

	// Run the exchange
	go run()
	defer shutdown()
	defer e.Shutdown()

	// Wait until we find 2 confirmed deposits
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-time.After(dbCheckWaitTime):
				confirmed, err := e.store.GetDepositInfoArray(filter)
				require.NoError(t, err)
				if len(confirmed) != len(dis) {
					continue
				}

				// Verify the 2 confirmed deposits
				expectedDis := make([]DepositInfo, len(dis))
				for i, di := range dis {
					expectedDis[i] = di
					expectedDis[i].Status = StatusDone

					if expectedDis[i].SkySent == 0 {
						amt, err := calculateSkyValue(di.Deposit.Value, e.cfg.Rate)
						require.NoError(t, err)
						expectedDis[i].SkySent = amt
					}

					require.NotEmpty(t, confirmed[i].UpdatedAt)
					expectedDis[i].UpdatedAt = confirmed[i].UpdatedAt

					require.Equal(t, expectedDis[i], confirmed[i])
				}

				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(dbScanTimeout):
		t.Fatal("Waiting for confirmed deposits timed out")
	}
}

func TestProcessUnconfirmedTx(t *testing.T) {
	// Tests that StatusWaitConfirm deposits found in the db are processed
	// on exchange startup.

	// Add StatusWaitConfirm deposits
	// They should all be confirmed after shutdown
	dis := []DepositInfo{
		{
			Seq:        1,
			Status:     StatusWaitConfirm,
			SkyAddress: "foo-sky-addr-1",
			BtcAddress: "foo-btc-addr-1",
			BtcTx:      "foo-tx-1:1",
			Txid:       "foo-sky-txid-1",
			SkySent:    100e8,
			SkyBtcRate: testSkyBtcRate,
			Deposit: scanner.Deposit{
				Address: "foo-btc-addr-1",
				Value:   1e8,
				Height:  20,
				Tx:      "foo-tx-1",
				N:       1,
			},
		},
		{
			Seq:        2,
			Status:     StatusWaitConfirm,
			SkyAddress: "foo-sky-addr-2",
			BtcAddress: "foo-btc-addr-2",
			BtcTx:      "foo-tx-2:2",
			Txid:       "foo-sky-txid-2",
			SkySent:    100e8,
			SkyBtcRate: testSkyBtcRate,
			Deposit: scanner.Deposit{
				Address: "foo-btc-addr-2",
				Value:   1e8,
				Height:  20,
				Tx:      "foo-tx-2",
				N:       2,
			},
		},
	}

	testRunProcessDepositBacklog(t, dis, func(e *Exchange, di DepositInfo) {
		e.sender.(*dummySender).setTxConfirmed(di.Txid)
	})
}

func TestProcessWaitSendDeposits(t *testing.T) {
	// Tests that StatusWaitSend deposits found in the db are processed
	// on exchange startup

	// Add StatusWaitSend deposits
	// They should all be confirmed after shutdown
	dis := []DepositInfo{
		{
			Seq:        1,
			Status:     StatusWaitSend,
			SkyAddress: "foo-sky-addr-1",
			BtcAddress: "foo-btc-addr-1",
			BtcTx:      "foo-tx-1:1",
			SkyBtcRate: testSkyBtcRate,
			Deposit: scanner.Deposit{
				Address: "foo-btc-addr-1",
				Value:   1e8,
				Height:  20,
				Tx:      "foo-tx-1",
				N:       1,
			},
		},
		{
			Seq:        2,
			Status:     StatusWaitSend,
			SkyAddress: "foo-sky-addr-2",
			BtcAddress: "foo-btc-addr-2",
			BtcTx:      "foo-tx-2:2",
			SkyBtcRate: testSkyBtcRate,
			Deposit: scanner.Deposit{
				Address: "foo-btc-addr-2",
				Value:   1e8,
				Height:  20,
				Tx:      "foo-tx-2",
				N:       2,
			},
		},
	}

	testRunProcessDepositBacklog(t, dis, func(e *Exchange, di DepositInfo) {
		err := e.store.BindAddress(di.SkyAddress, di.BtcAddress)
		require.NoError(t, err)
		e.sender.(*dummySender).addTxidResponse(di.Txid)
		e.sender.(*dummySender).setTxConfirmed(di.Txid)
	})
}

func TestSaveIncomingDepositCreateDepositFailed(t *testing.T) {
	// Tests that we log a message and continue if saveIncomingDeposit fails
	e, shutdown, hook := runExchangeMockStore(t)
	defer shutdown()
	defer e.Shutdown()

	btcAddr := "foo-btc-addr"
	txid := "foo-sky-txid"
	e.sender.(*dummySender).addTxidResponse(txid)

	dn := scanner.DepositNote{
		Deposit: scanner.Deposit{
			Address: btcAddr,
			Value:   1e8,
			Height:  20,
			Tx:      "foo-tx",
			N:       2,
		},
		ErrC: make(chan error, 1),
	}
	e.scanner.(*dummyScanner).addDeposit(dn)

	// Configure database mocks

	// GetDepositInfoArray is called twice on startup
	e.store.(*MockStore).On("GetDepositInfoArray", mock.MatchedBy(func(filt DepositFilter) bool {
		return true
	})).Return(nil, nil).Twice()

	// Return error on GetOrCreateDepositInfo
	createDepositErr := errors.New("GetOrCreateDepositInfo failed")
	e.store.(*MockStore).On("GetOrCreateDepositInfo", dn.Deposit, testSkyBtcRate).Return(DepositInfo{}, createDepositErr)

	// First loop calls saveIncomingDeposit
	// err is written to ErrC after this method finishes
	err := <-dn.ErrC
	require.Error(t, err)
	require.Equal(t, createDepositErr, err)

	// Check that we logged the failed save, so that we can recover it later
	logEntry := hook.LastEntry()
	require.Equal(t, logEntry.Message, "saveIncomingDeposit failed. This deposit will not be reprocessed until teller is restarted.")
	loggedDeposit := logEntry.Data["deposit"].(scanner.Deposit)
	require.Equal(t, dn.Deposit, loggedDeposit)
}

func TestProcessWaitSendDepositFailed(t *testing.T) {
	// Tests that we log a message and continue if processWaitSendDeposit fails
	e, shutdown, hook := runExchangeMockStore(t)
	defer shutdown()
	defer e.Shutdown()

	skyAddr := "foo-sky-addr"
	btcAddr := "foo-btc-addr"
	txid := "foo-sky-txid"
	e.sender.(*dummySender).addTxidResponse(txid)

	dn := scanner.DepositNote{
		Deposit: scanner.Deposit{
			Address: btcAddr,
			Value:   1e8,
			Height:  20,
			Tx:      "foo-tx",
			N:       2,
		},
		ErrC: make(chan error, 1),
	}
	e.scanner.(*dummyScanner).addDeposit(dn)

	// Configure database mocks

	// GetDepositInfoArray is called twice on startup
	e.store.(*MockStore).On("GetDepositInfoArray", mock.MatchedBy(func(filt DepositFilter) bool {
		return true
	})).Return(nil, nil).Twice()

	// GetBindAddress returns a bound address
	e.store.(*MockStore).On("GetBindAddress", btcAddr).Return(skyAddr, nil)

	// GetOrCreateDepositInfo returns a valid DepositInfo
	di := DepositInfo{
		Seq:        1,
		Status:     StatusWaitSend,
		SkyAddress: skyAddr,
		BtcAddress: btcAddr,
		BtcTx:      dn.Deposit.TxN(),
		SkyBtcRate: testSkyBtcRate,
		Deposit:    dn.Deposit,
	}
	e.store.(*MockStore).On("GetOrCreateDepositInfo", dn.Deposit, testSkyBtcRate).Return(di, nil)

	// sender.Send() succeeds
	e.sender.(*dummySender).addTxidResponse(di.Txid)

	// UpdateDepositInfo fails
	updateDepositInfoErr := errors.New("UpdateDepositInfo error")
	e.store.(*MockStore).On("UpdateDepositInfo", di.BtcTx, mock.MatchedBy(func(f func(DepositInfo) DepositInfo) bool {
		return true
	})).Return(DepositInfo{}, updateDepositInfoErr)

	// First loop calls saveIncomingDeposit
	// nil is written to ErrC after this method finishes
	err := <-dn.ErrC
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-time.After(dbCheckWaitTime):
				logEntry := hook.LastEntry()
				require.Equal(t, logEntry.Message, "processWaitSendDeposit failed. This deposit will not be reprocessed until teller is restarted.")
				loggedDepositInfo := logEntry.Data["depositInfo"].(DepositInfo)
				require.Equal(t, di, loggedDepositInfo)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(dbScanTimeout):
		t.Fatal("Waiting for deposit send failure timed out")
	}
}

// TODO -- mock exchange.Store and test error conditions