// Skycoin teller, which provides service of monitoring the bitcoin deposite
// and sending skycoin coins
package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/boltdb/bolt"
	btcrpcclient "github.com/btcsuite/btcd/rpcclient"
	"github.com/facebookgo/pidfile"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"github.com/skycoin/teller/src/addrs"
	"github.com/skycoin/teller/src/config"
	"github.com/skycoin/teller/src/exchange"
	"github.com/skycoin/teller/src/monitor"
	"github.com/skycoin/teller/src/scanner"
	"github.com/skycoin/teller/src/sender"
	"github.com/skycoin/teller/src/teller"
	"github.com/skycoin/teller/src/util/logger"
)

var (
	gitCommit = ""
)

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func createBtcScanner(log logrus.FieldLogger, cfg config.Config, scanStore *scanner.Store) (*scanner.BTCScanner, error) {
	certs, err := ioutil.ReadFile(cfg.BtcRPC.Cert)
	if err != nil {
		return nil, fmt.Errorf("Failed to read cfg.BtcRPC.Cert %s: %v", cfg.BtcRPC.Cert, err)
	}

	log.Info("Connecting to btcd")

	btcrpc, err := btcrpcclient.New(&btcrpcclient.ConnConfig{
		Endpoint:     "ws",
		Host:         cfg.BtcRPC.Server,
		User:         cfg.BtcRPC.User,
		Pass:         cfg.BtcRPC.Pass,
		Certificates: certs,
	}, nil)
	if err != nil {
		log.WithError(err).Error("Connect to btcd failed")
		return nil, err
	}

	log.Info("Connect to btcd succeeded")

	err = scanStore.AddSupportedCoin(config.CoinTypeBTC)
	if err != nil {
		log.WithError(err).Error("scanStore.AddSupportedCoin(config.CoinTypeBTC) failed")
		return nil, err
	}

	btcScanner, err := scanner.NewBTCScanner(log, scanStore, btcrpc, scanner.Config{
		ScanPeriod:            cfg.BtcScanner.ScanPeriod,
		ConfirmationsRequired: cfg.BtcScanner.ConfirmationsRequired,
		InitialScanHeight:     cfg.BtcScanner.InitialScanHeight,
	})
	if err != nil {
		log.WithError(err).Error("Open scan service failed")
		return nil, err
	}
	return btcScanner, nil
}

func createEthScanner(log logrus.FieldLogger, cfg config.Config, scanStore *scanner.Store) (*scanner.ETHScanner, error) {
	ethrpc, err := scanner.NewEthClient(cfg.EthRPC.Server, cfg.EthRPC.Port)
	if err != nil {
		log.WithError(err).Error("Connect geth failed")
		return nil, err
	}

	err = scanStore.AddSupportedCoin(config.CoinTypeETH)
	if err != nil {
		log.WithError(err).Error("scanStore.AddSupportedCoin(config.CoinTypeETH) failed")
		return nil, err
	}

	ethScanner, err := scanner.NewETHScanner(log, scanStore, ethrpc, scanner.Config{
		ScanPeriod:            cfg.EthScanner.ScanPeriod,
		ConfirmationsRequired: cfg.EthScanner.ConfirmationsRequired,
		InitialScanHeight:     cfg.EthScanner.InitialScanHeight,
	})
	if err != nil {
		log.WithError(err).Error("Open ethscan service failed")
		return nil, err
	}
	return ethScanner, nil
}

// createSkyScanner returns a new sky scanner instance
func createSkyScanner(log logrus.FieldLogger, cfg config.Config, scanStore *scanner.Store) (*scanner.SKYScanner, error) {
	skyrpc := scanner.NewSkyClient(cfg.SkyRPC.Address)
	err := scanStore.AddSupportedCoin(config.CoinTypeSKY)
	if err != nil {
		log.WithError(err).Error("scanStore.AddSupportedCoin(config.CoinTypeSKY) failed")
		return nil, err
	}

	skyScanner, err := scanner.NewSKYScanner(log, scanStore, skyrpc, scanner.Config{
		ScanPeriod:            cfg.SkyScanner.ScanPeriod,
		ConfirmationsRequired: cfg.EthScanner.ConfirmationsRequired,
		InitialScanHeight:     cfg.SkyScanner.InitialScanHeight,
	})
	if err != nil {
		log.WithError(err).Error("Open skyscan service failed")
		return nil, err
	}

	return skyScanner, nil
}

func createPidFile(log logrus.FieldLogger, cfg config.Config) error {
	// The pidfile will already be set if the user used -pidfile on the command line,
	// do not overwrite it in that case.
	if pidfile.GetPidfilePath() == "" {
		pidfile.SetPidfilePath(cfg.PidFilename)
	}

	// Skip if the pidfile is not configured
	if pidfile.GetPidfilePath() == "" {
		return nil
	}

	if err := pidfile.Write(); err != nil {
		log.WithError(err).Error("Failed to write pid file")
		return err
	}

	return nil
}

func run() error {
	cur, err := user.Current()
	if err != nil {
		fmt.Println("Failed to get user's home directory:", err)
		return err
	}
	defaultAppDir := filepath.Join(cur.HomeDir, ".teller-skycoin")

	appDirOpt := pflag.StringP("dir", "d", defaultAppDir, "application data directory")
	configNameOpt := pflag.StringP("config", "c", "config", "name of configuration file")
	pflag.Parse()

	if err := createFolderIfNotExist(*appDirOpt); err != nil {
		fmt.Println("Create application data directory failed:", err)
		return err
	}

	cfg, err := config.Load(*configNameOpt, *appDirOpt)
	if err != nil {
		return fmt.Errorf("Config error:\n%v", err)
	}

	cfg.GitCommit = gitCommit
	cfg.StartTime = time.Now()

	// Init logger
	rusloggger, err := logger.NewLogger(cfg.LogFilename, cfg.Debug)
	if err != nil {
		fmt.Println("Failed to create Logrus logger:", err)
		return err
	}

	log := rusloggger.WithField("prefix", "teller")

	log.WithField("config", cfg.Redacted()).Info("Loaded teller config")

	quit := make(chan struct{})
	go catchInterrupt(quit)

	// Open db
	dbPath := filepath.Join(*appDirOpt, cfg.DBFilename)
	db, err := bolt.Open(dbPath, 0700, &bolt.Options{
		Timeout: 1 * time.Second,
	})
	if err != nil {
		log.WithError(err).Error("Open db failed")
		return err
	}

	// Create pid file. Do this after trying to open the db, which has a file lock on it,
	// so only one teller instance can run with the same db.
	if err := createPidFile(log, cfg); err != nil {
		log.WithError(err).Error("createPidFile failed")
		return err
	}

	errC := make(chan error, 20)
	var wg sync.WaitGroup

	background := func(name string, errC chan<- error, f func() error) {
		log.Infof("Backgrounding task %s", name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f()
			if err != nil {
				log.WithError(err).Errorf("Backgrounded task %s failed", name)
				errC <- fmt.Errorf("Backgrounded task %s failed: %v", name, err)
			} else {
				log.Infof("Backgrounded task %s shutdown", name)
			}
		}()
	}

	var btcScanner *scanner.BTCScanner
	var ethScanner *scanner.ETHScanner
	var skyScanner *scanner.SKYScanner
	var scanService scanner.Scanner
	var scanEthService scanner.Scanner
	var scanSkyService scanner.Scanner
	var sendService *sender.SendService
	var sendRPC sender.Sender
	var btcAddrMgr *addrs.Addrs
	var ethAddrMgr *addrs.Addrs
	var skyAddrMgr *addrs.Addrs

	//create multiplexer to manage scanner
	multiplexer := scanner.NewMultiplexer(log)

	dummyMux := http.NewServeMux()

	// create scan storer
	scanStore, err := scanner.NewStore(log, db)
	if err != nil {
		log.WithError(err).Error("scanner.NewStore failed")
		return err
	}

	if cfg.Dummy.Scanner {
		log.Info("btcd disabled, running dummy scanner")
		scanService = scanner.NewDummyScanner(log)
		scanService.(*scanner.DummyScanner).RegisterCoinType(config.CoinTypeBTC)
		// TODO -- refactor dummy scanning to support multiple coin types
		// scanEthService = scanner.NewDummyScanner(log)
		scanService.(*scanner.DummyScanner).BindHandlers(dummyMux)
	} else {
		// enable btc scanner
		if cfg.BtcScanner.Enabled {
			btcScanner, err = createBtcScanner(rusloggger, cfg, scanStore)
			if err != nil {
				log.WithError(err).Error("create btc scanner failed")
				return err
			}
			background("btcScanner.Run", errC, btcScanner.Run)

			scanService = btcScanner
		}

		// enable eth scanner
		if cfg.EthScanner.Enabled {
			ethScanner, err = createEthScanner(rusloggger, cfg, scanStore)
			if err != nil {
				log.WithError(err).Error("create eth scanner failed")
				return err
			}

			background("ethScanner.Run", errC, ethScanner.Run)

			scanEthService = ethScanner

			if err := multiplexer.AddScanner(scanEthService, config.CoinTypeETH); err != nil {
				log.WithError(err).Errorf("multiplexer.AddScanner of %s failed", config.CoinTypeETH)
				return err
			}
		}

		if cfg.SkyScanner.Enabled {
			skyScanner, err = createSkyScanner(rusloggger, cfg, scanStore)
			if err != nil {
				log.WithError(err).Error("create sky scanner failed")
				return err
			}

			background("skyscanner.Run", errC, skyScanner.Run)

			scanSkyService = skyScanner

			if err := multiplexer.AddScanner(scanSkyService, config.CoinTypeSKY); err != nil {
				log.WithError(err).Errorf("multiplexer.AddScanner of %s failed", config.CoinTypeSKY)
				return err
			}
		}
	}

	if err := multiplexer.AddScanner(scanService, config.CoinTypeBTC); err != nil {
		log.WithError(err).Errorf("multiplexer.AddScanner of %s failed", config.CoinTypeBTC)
		return err
	}

	background("multiplex.Run", errC, multiplexer.Multiplex)

	if cfg.Dummy.Sender {
		log.Info("skyd disabled, running dummy sender")
		sendRPC = sender.NewDummySender(log)
		sendRPC.(*sender.DummySender).BindHandlers(dummyMux)
	} else {
		skyClient, err := sender.NewRPC(cfg.SkyExchanger.Wallet, cfg.SkyRPC.Address)
		if err != nil {
			log.WithError(err).Error("sender.NewRPC failed")
			return err
		}

		sendService = sender.NewService(log, skyClient)

		background("sendService.Run", errC, sendService.Run)

		sendRPC = sender.NewRetrySender(sendService)
	}

	if cfg.Dummy.Scanner || cfg.Dummy.Sender {
		log.Infof("Starting dummy admin interface listener on http://%s", cfg.Dummy.HTTPAddr)
		go func() {
			if err := http.ListenAndServe(cfg.Dummy.HTTPAddr, dummyMux); err != nil {
				log.WithError(err).Error("Dummy ListenAndServe failed")
			}
		}()
	}

	// create exchange service
	exchangeStore, err := exchange.NewStore(log, db)
	if err != nil {
		log.WithError(err).Error("exchange.NewStore failed")
		return err
	}

	var exchangeClient *exchange.Exchange

	switch cfg.SkyExchanger.BuyMethod {
	case config.BuyMethodDirect:
		var err error
		exchangeClient, err = exchange.NewDirectExchange(log, cfg.SkyExchanger, exchangeStore, multiplexer, sendRPC)
		if err != nil {
			log.WithError(err).Error("exchange.NewDirectExchange failed")
			return err
		}
	case config.BuyMethodPassthrough:
		var err error
		exchangeClient, err = exchange.NewPassthroughExchange(log, cfg.SkyExchanger, exchangeStore, multiplexer, sendRPC)
		if err != nil {
			log.WithError(err).Error("exchange.NewPassthroughExchange failed")
			return err
		}
	default:
		log.WithError(config.ErrInvalidBuyMethod).Error()
		return config.ErrInvalidBuyMethod
	}

	background("exchangeClient.Run", errC, exchangeClient.Run)

	// create AddrManager
	addrManager := addrs.NewAddrManager()

	if cfg.BtcScanner.Enabled {
		// create bitcoin address manager
		btcAddrMgr, err = addrs.NewBTCAddrs(log, db, cfg.BtcAddresses)
		if err != nil {
			log.WithError(err).Error("Create BTC deposit address manager failed")
			return err
		}
		if err := addrManager.PushGenerator(btcAddrMgr, config.CoinTypeBTC); err != nil {
			log.WithError(err).Error("Add BTC address manager failed")
			return err
		}
	}

	if cfg.EthScanner.Enabled {
		// create ethereum address manager
		ethAddrMgr, err = addrs.NewETHAddrs(log, db, cfg.EthAddresses)
		if err != nil {
			log.WithError(err).Error("Create ETH deposit address manager failed")
			return err
		}
		if err := addrManager.PushGenerator(ethAddrMgr, config.CoinTypeETH); err != nil {
			log.WithError(err).Error("Add ETH address manager failed")
			return err
		}
	}

	if cfg.SkyScanner.Enabled {
		// create sky address manager
		skyAddrMgr, err = addrs.NewSKYAddrs(log, db, cfg.SkyAddresses)
		if err != nil {
			log.WithError(err).Error("Create SKY deposit address manager failed")
			return err
		}
		if err := addrManager.PushGenerator(skyAddrMgr, config.CoinTypeSKY); err != nil {
			log.WithError(err).Error("Add SKY address manager failed")
			return err
		}
	}
	tellerServer := teller.New(log, exchangeClient, addrManager, cfg)

	// Run the service
	background("tellerServer.Run", errC, tellerServer.Run)
	// Start monitor service
	monitorService := monitor.New(log, cfg, addrManager, exchangeClient, scanStore, db)
	background("monitorService.Run", errC, monitorService.Run)

	var finalErr error
	select {
	case <-quit:
	case finalErr = <-errC:
		if finalErr != nil {
			log.WithError(finalErr).Error("Goroutine error")
		}
	}

	log.Info("Shutting down...")

	if monitorService != nil {
		log.Info("Shutting down monitorService")
		monitorService.Shutdown()
	}

	// close the teller service
	log.Info("Shutting down tellerServer")
	tellerServer.Shutdown()

	log.Info("Shutting down the multiplexer")
	multiplexer.Shutdown()

	// close the scan service
	if btcScanner != nil {
		log.Info("Shutting down btcScanner")
		btcScanner.Shutdown()
	}
	// close the scan service
	if ethScanner != nil {
		log.Info("Shutting down ethScanner")
		ethScanner.Shutdown()
	}

	// close exchange service
	log.Info("Shutting down exchangeClient")
	exchangeClient.Shutdown()

	// close the skycoin send service
	if sendService != nil {
		log.Info("Shutting down sendService")
		sendService.Shutdown()
	}

	log.Info("Waiting for goroutines to exit")

	wg.Wait()

	log.Info("Shutdown complete")

	return finalErr
}

func createFolderIfNotExist(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// create the dir
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
	}
	return nil
}

func printProgramStatus() {
	p := pprof.Lookup("goroutine")
	if err := p.WriteTo(os.Stdout, 2); err != nil {
		fmt.Println("ERROR:", err)
		return
	}
}

func catchInterrupt(quit chan<- struct{}) {
	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt)
	<-sigchan
	signal.Stop(sigchan)
	close(quit)

	// If ctrl-c is called again, panic so that the program state can be examined.
	// Ctrl-c would be called again if program shutdown was stuck.
	go catchInterruptPanic()
}

// catchInterruptPanic catches os.Interrupt and panics
func catchInterruptPanic() {
	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt)
	<-sigchan
	signal.Stop(sigchan)
	printProgramStatus()
	panic("SIGINT")
}
