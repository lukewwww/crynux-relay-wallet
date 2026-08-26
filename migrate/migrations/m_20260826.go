package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

type vestingRecordForM20260826 struct {
	ID     uint
	Status int8
}

func (vestingRecordForM20260826) TableName() string {
	return "vesting_records"
}

func M20260826(db *gorm.DB) *gormigrate.Gormigrate {
	return gormigrate.New(db, gormigrate.DefaultOptions, []*gormigrate.Migration{
		{
			ID: "M20260826",
			Migrate: func(tx *gorm.DB) error {
				return tx.Model(&vestingRecordForM20260826{}).
					Where("deleted_at IS NULL AND status = ?", 0).
					Update("status", 2).Error
			},
			Rollback: func(tx *gorm.DB) error {
				return nil
			},
		},
	})
}
