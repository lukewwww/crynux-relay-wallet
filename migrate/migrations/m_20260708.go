package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

func M20260708(db *gorm.DB) *gormigrate.Gormigrate {
	type VestingRecord struct {
		Source     string `gorm:"not null;size:64;index"`
		ExternalID string `gorm:"not null;size:128;index"`
	}

	return gormigrate.New(db, gormigrate.DefaultOptions, []*gormigrate.Migration{
		{
			ID: "M20260708",
			Migrate: func(tx *gorm.DB) error {
				if err := tx.Migrator().DropIndex(&VestingRecord{}, "Source"); err != nil {
					return err
				}
				if err := tx.Migrator().DropIndex(&VestingRecord{}, "ExternalID"); err != nil {
					return err
				}
				if err := tx.Migrator().DropColumn(&VestingRecord{}, "Source"); err != nil {
					return err
				}
				if err := tx.Migrator().DropColumn(&VestingRecord{}, "ExternalID"); err != nil {
					return err
				}
				return nil
			},
			Rollback: func(tx *gorm.DB) error {
				if err := tx.Migrator().AddColumn(&VestingRecord{}, "Source"); err != nil {
					return err
				}
				if err := tx.Migrator().AddColumn(&VestingRecord{}, "ExternalID"); err != nil {
					return err
				}
				if err := tx.Migrator().CreateIndex(&VestingRecord{}, "Source"); err != nil {
					return err
				}
				if err := tx.Migrator().CreateIndex(&VestingRecord{}, "ExternalID"); err != nil {
					return err
				}
				return nil
			},
		},
	})
}
