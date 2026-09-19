package model

import "github.com/zeromicro/go-zero/core/stores/sqlx"

var _ AfvUserAgreementModel = (*customAfvUserAgreementModel)(nil)

type (
	// AfvUserAgreementModel is an interface to be customized, add more methods here,
	// and implement the added methods in customAfvUserAgreementModel.
	AfvUserAgreementModel interface {
		afvUserAgreementModel
		withSession(session sqlx.Session) AfvUserAgreementModel
	}

	customAfvUserAgreementModel struct {
		*defaultAfvUserAgreementModel
	}
)

// NewAfvUserAgreementModel returns a model for the database table.
func NewAfvUserAgreementModel(conn sqlx.SqlConn) AfvUserAgreementModel {
	return &customAfvUserAgreementModel{
		defaultAfvUserAgreementModel: newAfvUserAgreementModel(conn),
	}
}

func (m *customAfvUserAgreementModel) withSession(session sqlx.Session) AfvUserAgreementModel {
	return NewAfvUserAgreementModel(sqlx.NewSqlConnFromSession(session))
}
