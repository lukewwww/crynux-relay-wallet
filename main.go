package main

import (
	"context"
	"crynux_relay_wallet/alert"
	"crynux_relay_wallet/blockchain"
	"crynux_relay_wallet/config"
	"crynux_relay_wallet/migrate"
	"crynux_relay_wallet/tasks"
	"fmt"
	"os"
	"os/signal"
	"sync"

	log "github.com/sirupsen/logrus"
)

func main() {

	log.Infoln("Starting Crynux Relay Wallet...")

	log.Infoln("Initializing config...")
	if err := config.InitConfig(""); err != nil {
		print("Error reading config file")
		print(err.Error())
		os.Exit(1)
	}

	conf := config.GetConfig()

	log.Infoln("Initializing log...")
	if err := config.InitLog(conf); err != nil {
		print("Error initializing log")
		print(err.Error())
		os.Exit(1)
	}

	log.Infoln("Initializing database...")
	if err := config.InitDB(conf); err != nil {
		log.Errorln(err.Error())
		os.Exit(1)
	}

	log.Infoln("Starting database migration...")
	startDBMigration()
	log.Infoln("DB migrations are done!")

	log.Infoln("Initializing blockchain...")
	if err := blockchain.Init(context.Background()); err != nil {
		log.Fatalln(err)
	}
	log.Infoln("Start blockchain transaction manager...")
	tm := blockchain.NewTransactionManager(config.GetDB())
	if err := tm.Start(context.Background()); err != nil {
		log.Fatalf("Failed to start blockchain transaction manager: %v", err)
	}

	log.Infoln("Starting tasks...")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var wg sync.WaitGroup

	runTask := func(taskName string, taskFunc func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Errorln(fmt.Sprintf("[Task:%s] panic: %v", taskName, recovered))
					alert.SafeSendAlert(taskName, fmt.Sprintf("[Task:%s] panic: %v", taskName, recovered))
				}
				wg.Done()
			}()
			if err := taskFunc(ctx); err != nil {
				log.Errorln(fmt.Sprintf("[Task:%s] error: %v", taskName, err))
				alert.SafeSendAlert(taskName, fmt.Sprintf("[Task:%s] error: %v", taskName, err))
			}
		}()
	}

	runTask("Heartbeat", tasks.StartHeartbeat)
	runTask("SyncTaskFeeLogs", tasks.StartSyncTaskFeeLogs)
	runTask("SyncWithdrawalRequests", tasks.StartSyncWithdrawalRequests)
	runTask("ProcessWithdrawalRequests", tasks.StartProcessWithdrawalRequests)

	<-ctx.Done()
	log.Infoln("Shutdown signal received, stopping tasks...")
	wg.Wait()
	log.Infoln("All tasks stopped")
}

func startDBMigration() {

	migrate.InitMigration(config.GetDB())

	if err := migrate.Migrate(); err != nil {
		log.Errorln(err.Error())
		if err = migrate.Rollback(); err != nil {
			log.Errorln(err.Error())
		}
		os.Exit(1)
	}
}
