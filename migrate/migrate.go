package migrate

import (
	"crynux_relay_wallet/migrate/migrations"

	"github.com/go-gormigrate/gormigrate/v2"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

var migrationScripts []*gormigrate.Gormigrate

func Migrate() error {
	for _, migrationScript := range migrationScripts {
		if err := migrationScript.Migrate(); err != nil {
			log.Errorf("Migrate: %v", err)
			return err
		}
	}

	return nil
}

func Rollback() error {
	lastMigration := migrationScripts[len(migrationScripts)-1]

	if err := lastMigration.RollbackLast(); err != nil {
		log.Errorf("Migrate: %v", err)
		return err
	}

	return nil
}

func InitMigration(db *gorm.DB) {
	db.Set("gorm:table_options", "CHARSET=utf8mb4")

	// Add new migrations here
	migrationScripts = append(migrationScripts, migrations.M20250811(db))
	migrationScripts = append(migrationScripts, migrations.M20250902(db))
	migrationScripts = append(migrationScripts, migrations.M20250930(db))
	migrationScripts = append(migrationScripts, migrations.M20260327(db))
	migrationScripts = append(migrationScripts, migrations.M20260526(db))
	migrationScripts = append(migrationScripts, migrations.M20260602(db))
	migrationScripts = append(migrationScripts, migrations.M20260609(db))
	migrationScripts = append(migrationScripts, migrations.M20260708(db))
	migrationScripts = append(migrationScripts, migrations.M20260803(db))
	migrationScripts = append(migrationScripts, migrations.M20260803b(db))
	migrationScripts = append(migrationScripts, migrations.M20260803c(db))
	migrationScripts = append(migrationScripts, migrations.M20260826(db))
}
