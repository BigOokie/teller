package exchange

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sirupsen/logrus"
	"github.com/skycoin/exchange-api/exchange/c2cx"
	"github.com/skycoin/skycoin/src/util/droplet"

	"github.com/skycoin/teller/src/config"
	"github.com/skycoin/teller/src/scanner"
)

/*

Passthrough is implemented by making "market" buy orders on c2cx.com

"market" orders allow one to specify an amount of BTC to spend, rather than
specifying an order in terms of SKY volume and price.

*/

const (
	checkOrderWait = time.Second * 2
)

var (
	// ErrFatalOrderStatus is returned if an order has a fatal status
	ErrFatalOrderStatus = errors.New("Fatal order status")

	errCompletedAmountNegative = errors.New("Calculated amount of SKY bought is unexpectedly negative")
	errQuit                    = errors.New("quit")
)

// Passthrough implements a Processor. For each deposit, it buys a corresponding amount
// from c2cx.com, then tells the sender to send the amount bought.
type Passthrough struct {
	log              logrus.FieldLogger
	cfg              config.SkyExchanger
	receiver         Receiver
	store            Storer
	internalDeposits chan DepositInfo
	deposits         chan DepositInfo
	quit             chan struct{}
	done             chan struct{}
	statusLock       sync.RWMutex
	status           error
	exchangeClient   C2CXClient
}

// C2CXClient defines an interface for c2cx.Client
type C2CXClient interface {
	GetOrderByStatus(c2cx.TradePair, c2cx.OrderStatus) ([]c2cx.Order, error)
	GetOrderInfo(c2cx.TradePair, c2cx.OrderID) (*c2cx.Order, error)
	MarketBuy(c2cx.TradePair, decimal.Decimal, *string) (c2cx.OrderID, error)
}

// NewPassthrough creates Passthrough
func NewPassthrough(log logrus.FieldLogger, cfg config.SkyExchanger, store Storer, receiver Receiver) (*Passthrough, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &Passthrough{
		log:              log.WithField("prefix", "teller.exchange.passthrough"),
		cfg:              cfg,
		store:            store,
		receiver:         receiver,
		internalDeposits: make(chan DepositInfo, 100),
		deposits:         make(chan DepositInfo, 100),
		quit:             make(chan struct{}),
		done:             make(chan struct{}, 1),
		exchangeClient: &c2cx.Client{
			Key:    cfg.C2CX.Key,
			Secret: cfg.C2CX.Secret,
			Debug:  false,
		},
	}, nil
}

// Run begins the Passthrough service
func (p *Passthrough) Run() error {
	log := p.log
	log.Info("Start passthrough service...")
	defer func() {
		log.Info("Closed passthrough service")
		p.done <- struct{}{}
	}()

	// Look for deposits that had an order placed, but for which we failed to record the OrderID
	// This could have occured if a DB save had failed or the process was interrupted at the wrong time.
	// Recovery will record the missing order data and set the status to StatusWaitPassthroughOrderComplete
	recoveredDeposits, err := p.fixUnrecordedOrders()
	if err != nil {
		log.WithError(err).Error("fixUnrecordedOrders failed")
		return err
	}

	if len(recoveredDeposits) > 0 {
		log.WithField("recoveredDeposits", len(recoveredDeposits)).Info("Recovered unrecorded orders for deposits")
	}

	// Load StatusWaitPassthrough and StatusWaitPassthroughOrderComplete deposits for reprocessing
	waitPassthroughDeposits, err := p.store.GetDepositInfoArray(func(di DepositInfo) bool {
		return di.Status == StatusWaitPassthrough
	})

	if err != nil {
		log.WithError(err).Error("GetDepositInfoArray failed")
		return err
	}

	waitPassthroughOrderCompleteDeposits, err := p.store.GetDepositInfoArray(func(di DepositInfo) bool {
		return di.Status == StatusWaitPassthroughOrderComplete
	})

	if err != nil {
		log.WithError(err).Error("GetDepositInfoArray failed")
		return err
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		p.runBuy()
	}()

	// Queue the saved StatusWaitPassthroughOrderComplete deposits
queueWaitPassthroughOrderCompleteDeposits:
	for _, di := range waitPassthroughOrderCompleteDeposits {
		select {
		case <-p.quit:
			break queueWaitPassthroughOrderCompleteDeposits
		case p.internalDeposits <- di:
		}
	}

queueWaitPassthroughDeposits:
	// Queue the saved StatusWaitPassthrough deposits
	for _, di := range waitPassthroughDeposits {
		select {
		case <-p.quit:
			break queueWaitPassthroughDeposits
		case p.internalDeposits <- di:
		}
	}

	// Merge receiver.Deposits() into the internal internalDeposits
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.receiveDeposits()
	}()

	wg.Wait()

	return nil
}

func (p *Passthrough) runBuy() {
	log := p.log.WithField("goroutine", "runBuy")
	for {
		select {
		case <-p.quit:
			log.Info("quit")
			return
		case d := <-p.internalDeposits:
			d, err := p.processWaitDecideDeposit(d)
			log := log.WithField("depositInfo", d)
			if err != nil {
				msg := "handleDeposit failed. This deposit will not be reprocessed until teller is restarted."
				log.WithError(err).Error(msg)
				continue
			}

			log.WithField("depositInfo", d).Info("Deposit processed")

			p.deposits <- d
		}
	}
}

func (p *Passthrough) receiveDeposits() {
	log := p.log.WithField("goroutine", "receiveDeposits")
	for {
		select {
		case <-p.quit:
			log.Info("quit")
			return
		case d := <-p.receiver.Deposits():
			log.WithField("depositInfo", d).Info("Received deposit from receiver")
			p.internalDeposits <- d
		}
	}
}

// Shutdown stops a previous call to Run
func (p *Passthrough) Shutdown() {
	p.log.Info("Shutting down Passthrough")
	close(p.quit)
	p.log.Info("Waiting for run to finish")
	<-p.done
	p.log.Info("Shutdown complete")
}

// Deposits returns a channel of processed deposits
func (p *Passthrough) Deposits() <-chan DepositInfo {
	return p.deposits
}

// processWaitDecideDeposit advances a single deposit through these states:
// StatusWaitDecide -> StatusWaitPassthrough
// StatusWaitPassthrough -> StatusWaitPassthroughOrderComplete
// StatusWaitPassthroughOrderComplete -> StatusWaitSend
func (p *Passthrough) processWaitDecideDeposit(di DepositInfo) (DepositInfo, error) {
	log := p.log.WithField("depositInfo", di)
	log.Info("Processing StatusWaitDecide deposit")

	for {
		select {
		case <-p.quit:
			return di, nil
		default:
		}

		log.Info("handleDepositInfoState")

		var err error
		di, err = p.handleDepositInfoState(di)
		log = log.WithField("depositInfo", di)

		p.setStatus(err)

		retry := "retry"
		retryRatelimited := "retry_ratelimited"
		fail := "fail"
		quit := "quit"

		var action string
		switch e := err.(type) {
		case c2cx.APIError:
			// Retry a c2cx.APIError by default
			action = retry

			// If the error is because the BTC volume for the order is too low, fail
			if strings.HasPrefix(e.Message, "limit value:") {
				action = fail
			}

			if e.Message == "Too Many Requests" {
				action = retryRatelimited
			}

		case c2cx.Error:
			// Retry any other c2cx.Error by default.
			// Includes net.Error, which can occur if the network or remote server are unavailable.
			// Includes a JSON parsing error, since sometimes the C2CX API will respond with XML.
			action = retry

		case net.Error:
			// Treat net.Error errors as temporary,
			action = retry

		default:
			switch err {
			case nil:
			case errQuit:
				action = quit
			default:
				action = fail
			}
		}

		if err != nil && err != errQuit {
			log.WithField("action", action).WithError(err).Error("handleDepositInfoState failed")
		}

		switch action {
		case retry:
			select {
			case <-time.After(p.cfg.C2CX.RequestFailureWait):
			case <-p.quit:
				return di, nil
			}
		case retryRatelimited:
			select {
			case <-time.After(p.cfg.C2CX.RatelimitWait):
			case <-p.quit:
				return di, nil
			}
		case fail:
			return di, err
		case quit:
			return di, nil
		}

		if di.Status == StatusWaitSend {
			return di, nil
		}
	}
}

func (p *Passthrough) handleDepositInfoState(di DepositInfo) (DepositInfo, error) {
	log := p.log.WithField("depositInfo", di)

	if err := di.ValidateForStatus(); err != nil {
		log.WithError(err).Error("handleDepositInfoState's DepositInfo is invalid")
		return di, err
	}

	if di.CoinType != scanner.CoinTypeBTC {
		log.WithError(scanner.ErrUnsupportedCoinType).Error("Only CoinTypeBTC deposits are accepted for passthrough")
		return di, scanner.ErrUnsupportedCoinType
	}

	switch di.Status {
	case StatusWaitDecide:
		// Set status to StatusWaitPassthrough
		di, err := p.store.UpdateDepositInfo(di.DepositID, func(di DepositInfo) DepositInfo {
			di.Status = StatusWaitPassthrough
			di.Passthrough.ExchangeName = PassthroughExchangeC2CX
			di.Passthrough.RequestedAmount = calculateRequestedAmount(di.DepositValue).String()
			di.Passthrough.Order.CustomerID = di.DepositID
			return di
		})
		if err != nil {
			log.WithError(err).Error("UpdateDepositInfo set StatusWaitPassthrough failed")
			return di, err
		}

		log.Info("DepositInfo status set to StatusWaitPassthrough")

		return di, nil

	case StatusWaitPassthrough:
		// Place a market order for the amount of BTC to spend.
		// NOTE: if the balance on the exchange is insufficient, the order will be "suspended"
		// until the balance is high enough.
		orderID, err := p.placeOrder(di)
		if err != nil {
			log.WithError(err).Error("placeOrder failed")
			return di, err
		}

		log = log.WithField("orderID", orderID)
		log.Info("Created order")

		// NOTE: if the DB update fails, the order had already been placed and we lost this info.
		// To handle this case, during startup, for any deposits of StatusWaitPassthrough,
		// we scan our orders on C2CX to see if any have a CustomerID matching our DepositID,
		// and update the DepositInfo in the database to recover.
		di, err = p.store.UpdateDepositInfo(di.DepositID, func(di DepositInfo) DepositInfo {
			di.Status = StatusWaitPassthroughOrderComplete
			di.Passthrough.Order.OrderID = fmt.Sprint(orderID)
			return di
		})
		if err != nil {
			log.WithError(err).Error("UpdateDepositInfo with initial order data failed")
			return di, err
		}

		log.Info("DepositInfo status set to StatusWaitPassthroughOrderComplete")

		return di, nil

	case StatusWaitPassthroughOrderComplete:
		newDepositInfo, err := p.waitOrderComplete(di)

		log = log.WithField("depositInfo", newDepositInfo)

		switch err {
		case nil:
		case errQuit:
			return di, err
		case ErrFatalOrderStatus:
			log.WithError(err).Error("Fatal order status")
		default:
			log.WithError(err).Error("waitOrderComplete failed")
			return di, err
		}

		di, err = p.store.UpdateDepositInfo(di.DepositID, func(di DepositInfo) DepositInfo {
			newDepositInfo.Status = StatusWaitSend
			return newDepositInfo
		})
		if err != nil {
			log.WithError(err).Error("UpdateDepositInfo set StatusWaitSend failed")
			return di, err
		}

		log.Info("DepositInfo status set to StatusWaitSend")

		return di, nil

	default:
		err := ErrDepositStatusInvalid
		log.WithError(err).Error(err)
		return di, err
	}
}

// fixUnrecordedOrders looks for incomplete orders already placed to clean up and resume
// in the event that exchange processing was interrupted
func (p *Passthrough) fixUnrecordedOrders() ([]DepositInfo, error) {
	// An order may have been placed with a deposit's CustomerID
	// without recording the OrderID, either due to a database save failure
	// or an unexpected interruption of the process.
	// Unforuntately we cannot search orders by CustomerID directly, and
	// have to scan all orders to find one matching the customer ID.
	// Here, we query all c2cx orders and see if any have a CID that matches
	// a DepositInfo whose status is StatusWaitPassthrough.
	var updates []DepositInfo

	// Check all orders on StatusWaitPassthrough, to see if the order had actually been placed.
	// The order can be placed but then fail to update the DB, and we should not place the order twice.
	deposits, err := p.store.GetDepositInfoArray(func(di DepositInfo) bool {
		return di.Status == StatusWaitPassthrough
	})
	if err != nil {
		p.log.WithError(err).Error("GetDepositInfoArray failed")
		return nil, err
	}

	if len(deposits) == 0 {
		p.log.Info("No StatusWaitPassthrough deposits found")
		return nil, nil
	}

	log := p.log.WithField("waitPassthroughDeposits", len(deposits))
	log.Info("Found StatusWaitPassthrough deposits")

	cidToDeposits := make(map[string]DepositInfo, len(deposits))
	for _, di := range deposits {
		if di.Passthrough.Order.CustomerID == "" {
			return nil, errors.New("StatusWaitPassthrough deposit unexpectedly does not have CustomerID set")
		}

		cidToDeposits[di.Passthrough.Order.CustomerID] = di
	}

	// TODO -- use the "duration" argument to filter orders since a certain time?
	// Is that how this parameter works?

	// Get all orders
	// If any's CID matches the DepositInfo's, update that DepositInfo
	log.Info("Calling GetOrderByStatus to recover placed orders")
	orders, err := p.exchangeClient.GetOrderByStatus(c2cx.BtcSky, c2cx.StatusAll)
	if err != nil {
		log.WithError(err).Error("exchangeClient.GetOrderByStatus(StatusAll) failed")
		return nil, err
	}

	for _, o := range orders {
		if o.CID == nil {
			continue
		}

		di, ok := cidToDeposits[*o.CID]
		if !ok {
			continue
		}

		// Update the DepositInfo
		di, err = p.store.UpdateDepositInfo(di.DepositID, func(di DepositInfo) DepositInfo {
			di.Status = StatusWaitPassthroughOrderComplete
			di.Passthrough.Order.OrderID = fmt.Sprint(o.OrderID)
			return di
		})
		if err != nil {
			log.WithError(err).Error("UpdateDepositInfo with initial order data failed")
			return nil, err
		}

		updates = append(updates, di)
	}

	return updates, nil
}

// placeOrder places an order on the exchange and returns the OrderID
func (p *Passthrough) placeOrder(di DepositInfo) (c2cx.OrderID, error) {
	if di.CoinType != scanner.CoinTypeBTC {
		return 0, scanner.ErrUnsupportedCoinType
	}

	// The CustomerID should be saved on the DepositInfo prior to calling placeOrder
	if di.Passthrough.Order.CustomerID == "" {
		return 0, errors.New("CustomerID is not set on DepositInfo.Passthrough")
	}

	amount, err := decimal.NewFromString(di.Passthrough.RequestedAmount)
	if err != nil {
		p.log.WithField("depositInfo", di).WithError(err).Error("Could not parse DepositInfo.RequestedAmount")
		return 0, err
	}

	customerID := di.Passthrough.Order.CustomerID

	orderID, err := p.exchangeClient.MarketBuy(c2cx.BtcSky, amount, &customerID)
	if err != nil {
		return 0, err
	}

	return orderID, nil
}

// waitOrderComplete checks an order's status, waiting until it reaches a terminal state
func (p *Passthrough) waitOrderComplete(di DepositInfo) (DepositInfo, error) {
	log := p.log.WithField("depositInfo", di)

	if di.Passthrough.Order.OrderID == "" {
		return di, errors.New("DepositInfo.Passthrough.OrderID is not set")
	}

	orderID, err := strconv.Atoi(di.Passthrough.Order.OrderID)
	if err != nil {
		log.WithError(err).Error("OrderID cannot be parsed to int")
		return di, err
	}

waitCompletedLoop:
	for {
		log.Debug("Waiting for order to complete")
		select {
		case <-p.quit:
			return di, errQuit
		case <-time.After(checkOrderWait):
			var err error
			order, err := p.exchangeClient.GetOrderInfo(c2cx.BtcSky, c2cx.OrderID(orderID))
			if err != nil {
				log.WithError(err).Error("exchangeClient.GetOrderInfo failed")
				return di, err
			}

			log = log.WithField("order", order)
			log = log.WithField("orderStatus", order.Status.String())
			log.Info("GetOrderInfo")

			// Don't trust the C2CX API
			if fmt.Sprint(order.OrderID) != di.Passthrough.Order.OrderID {
				err := errors.New("order.OrderID != di.Passthrough.OrderID unexpectedly")
				log.WithError(err).Error()
				return di, err
			}

			if order.CID == nil || *order.CID != di.Passthrough.Order.CustomerID {
				err := errors.New("order.CID != di.Passthrough.Order.CustomerID unexpectedly")
				log.WithError(err).Error()
				return di, err
			}

			switch order.Status {
			case c2cx.StatusPartial, c2cx.StatusPending, c2cx.StatusActive, c2cx.StatusSuspended, c2cx.StatusTriggerPending, c2cx.StatusStopLossPending:
				// Partial orders -- should complete eventually
				// Pending orders -- unknown
				// Active orders -- unsure, but assume should complete eventually
				// Suspended orders -- if balance is too low
				// TriggerPending and StopLossPending -- should never occur,
				// but in case they did, these are transitory states and not final states, so wait for them to complete
				log.Info("Order status has not finalized")
				continue waitCompletedLoop

			case c2cx.StatusCompleted:
				log.Info("Order completed")

				skyBought, err := calculateSkyBought(order)
				if err != nil {
					p.log.WithFields(logrus.Fields{
						"order":       order,
						"depositInfo": di,
					}).WithError(err).Error("calculateSkyBought failed, no coins will be sent")
					// Don't return here, continue and update the deposit info
					// The sender will reject a send of 0 sky later
				}

				btcSpent := calculateBtcSpent(order)

				di.Passthrough.SkyBought = skyBought
				di.Passthrough.DepositValueSpent = btcSpent

				di.Passthrough.Order.Status = order.Status.String()
				di.Passthrough.Order.Final = true
				di.Passthrough.Order.Original = *order

				di.Passthrough.Order.CompletedAmount = order.CompletedAmount.String()
				di.Passthrough.Order.Price = order.AvgPrice.String()

				return di, nil

			default:
				log.WithError(ErrFatalOrderStatus).Error("Fatal status encountered")
				di.Passthrough.Order.Status = order.Status.String()
				di.Passthrough.Order.Final = true
				di.Passthrough.Order.Original = *order
				return di, ErrFatalOrderStatus
			}
		}
	}

	return di, nil
}

// calculateRequestedAmount converts the amount of satoshis to a decimal amount, truncated to the maximum
// precision allowed by the c2cx API for this orderbook
func calculateRequestedAmount(depositValue int64) decimal.Decimal {
	amount := decimal.New(depositValue, -int32(SatoshiExponent))
	amount = amount.Truncate(int32(c2cx.TradePairRulesTable[c2cx.BtcSky].PricePrecision))
	return amount
}

// calculateSkyBought returns the amount of SKY bought in droplets
// The amount of SKY bought is in order.CompletedAmount
// This amount does is not adjusted for the C2CX commission, which is not
// known through the API, so the actual amount bought is less.
// For now, ignore the commission and eat the fee.
func calculateSkyBought(order *c2cx.Order) (uint64, error) {
	// Convert CompletedAmount from whole skycoin to satoshis
	skyBought := order.CompletedAmount.Mul(decimal.New(droplet.Multiplier, 0)).IntPart()
	if skyBought < 0 {
		return 0, errCompletedAmountNegative
	}
	return uint64(skyBought), nil
}

// calculateBtcSpent returns the amount of BTC spent in satoshis.
// The amount spent can be less than the amount requested to be spent, due to the
// minimum BTC price of the smallest purchasable unit of SKY on the exchange.
func calculateBtcSpent(order *c2cx.Order) int64 {
	btcSpentDec := order.CompletedAmount.Mul(order.AvgPrice)
	return btcSpentDec.Mul(decimal.New(SatoshisPerBTC, 0)).IntPart()
}

func (p *Passthrough) setStatus(err error) {
	defer p.statusLock.Unlock()
	p.statusLock.Lock()
	p.status = err
}

// Status returns the last return value of the processing state
func (p *Passthrough) Status() error {
	defer p.statusLock.RUnlock()
	p.statusLock.RLock()
	return p.status
}
