package migrations

import (
	"database/sql"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

type withdrawAuthorizationFieldsForM20260803c struct {
	Timestamp                sql.NullInt64  `gorm:"null"`
	Signature                sql.NullString `gorm:"type:varchar(255);null"`
	AuthorizationFingerprint sql.NullString `gorm:"type:varchar(64);null;uniqueIndex"`
}

func (withdrawAuthorizationFieldsForM20260803c) TableName() string {
	return "withdraw_records"
}

func M20260803c(db *gorm.DB) *gormigrate.Gormigrate {
	return gormigrate.New(db, gormigrate.DefaultOptions, []*gormigrate.Migration{
		{
			ID: "M20260803c",
			Migrate: func(tx *gorm.DB) error {
				if err := tx.Migrator().AddColumn(&withdrawAuthorizationFieldsForM20260803c{}, "Timestamp"); err != nil {
					return err
				}
				if err := tx.Migrator().AddColumn(&withdrawAuthorizationFieldsForM20260803c{}, "Signature"); err != nil {
					return err
				}
				if err := tx.Migrator().AddColumn(&withdrawAuthorizationFieldsForM20260803c{}, "AuthorizationFingerprint"); err != nil {
					return err
				}
				return tx.Migrator().CreateIndex(&withdrawAuthorizationFieldsForM20260803c{}, "AuthorizationFingerprint")
			},
			Rollback: func(tx *gorm.DB) error {
				if err := tx.Migrator().DropIndex(&withdrawAuthorizationFieldsForM20260803c{}, "AuthorizationFingerprint"); err != nil {
					return err
				}
				if err := tx.Migrator().DropColumn(&withdrawAuthorizationFieldsForM20260803c{}, "AuthorizationFingerprint"); err != nil {
					return err
				}
				if err := tx.Migrator().DropColumn(&withdrawAuthorizationFieldsForM20260803c{}, "Signature"); err != nil {
					return err
				}
				return tx.Migrator().DropColumn(&withdrawAuthorizationFieldsForM20260803c{}, "Timestamp")
			},
		},
	})
}
