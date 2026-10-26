package data

import (
	"database/sql"
	"errors"
)

var (
	ErrRecordNotFound = errors.New("record not found")
	ErrEditConflict   = errors.New("edit conflict")
)

type Models struct {
	Users     UserModel
	APIKey    APIKeyModel
	LLMModels LLMModelModel
}

func NewModels(db *sql.DB) Models {
	return Models{
		Users:     UserModel{DB: db},
		APIKey:    APIKeyModel{DB: db},
		LLMModels: LLMModelModel{DB: db},
	}
}
