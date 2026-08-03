package relay_api

import (
	"bytes"
	"context"
	"crynux_relay_wallet/config"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	log "github.com/sirupsen/logrus"
)

type WithdrawStatus int8

const (
	WithdrawStatusPending WithdrawStatus = iota
	WithdrawStatusSuccess
	WithdrawStatusFailed
)

type WithdrawalRequest struct {
	ID                  uint           `json:"id"`
	CreatedAt           uint64         `json:"created_at"`
	Address             string         `json:"address"`
	BenefitAddress      string         `json:"benefit_address"`
	Amount              string         `json:"amount"`
	WithdrawalFee       string         `json:"withdrawal_fee"`
	Network             string         `json:"network"`
	Status              WithdrawStatus `json:"status"`
	RelayAccountEventID uint           `json:"relay_account_event_id"`
	Timestamp           int64          `json:"timestamp"`
	Signature           string         `json:"signature"`
}

type GetWithdrawalRequestsInput struct {
	StartID uint `query:"start_id" json:"start_id" description:"Start ID"`
	Limit   int  `query:"limit" json:"limit" description:"Limit"`
}

type GetWithdrawalRequestsOutput struct {
	Data []WithdrawalRequest `json:"data"`
}

func GetWithdrawalRequests(ctx context.Context, pivotWithdrawalRequestID uint, limit int) ([]WithdrawalRequest, error) {
	conf := config.GetConfig()

	req, err := http.NewRequest("GET", conf.Relay.Api.Host+"/v1/withdraw/list", nil)
	if err != nil {
		return nil, err
	}

	input := GetWithdrawalRequestsInput{
		StartID: pivotWithdrawalRequestID,
		Limit:   limit,
	}

	timestamp, signature, err := SignData(input, conf.Relay.Api.PrivateKey)
	if err != nil {
		return nil, err
	}

	q := req.URL.Query()
	q.Add("start_id", fmt.Sprintf("%d", pivotWithdrawalRequestID))
	q.Add("limit", fmt.Sprintf("%d", limit))
	q.Add("timestamp", fmt.Sprintf("%d", timestamp))
	q.Add("signature", signature)
	req.URL.RawQuery = q.Encode()

	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Errorln(err)
		}
	}(resp.Body)

	if err := processRelayResponse(resp); err != nil {
		return nil, err
	}

	var output GetWithdrawalRequestsOutput

	if err := json.NewDecoder(resp.Body).Decode(&output); err != nil {
		return nil, err
	}

	return output.Data, nil
}

type FulfillWithdrawalRequestInput struct {
	ID     uint   `json:"id"`
	TxHash string `json:"tx_hash" description:"Transaction hash"`
}

type FulfillWithdrawalRequestInputWithSignature struct {
	FulfillWithdrawalRequestInput
	Timestamp int64  `form:"timestamp" json:"timestamp" description:"Signature timestamp" validate:"required"`
	Signature string `form:"signature" json:"signature" description:"Signature" validate:"required"`
}

func FulfillWithdrawalRequest(ctx context.Context, withdrawalRequestID uint, txHash string) error {
	conf := config.GetConfig()

	input := FulfillWithdrawalRequestInput{
		ID:     withdrawalRequestID,
		TxHash: txHash,
	}

	timestamp, signature, err := SignData(input, conf.Relay.Api.PrivateKey)
	if err != nil {
		return err
	}

	inputWithSignature := FulfillWithdrawalRequestInputWithSignature{
		FulfillWithdrawalRequestInput: input,
		Timestamp:                     timestamp,
		Signature:                     signature,
	}

	body, err := json.Marshal(inputWithSignature)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/v1/withdraw/%d/fulfill", conf.Relay.Api.Host, withdrawalRequestID), bytes.NewBuffer(body))
	if err != nil {
		return err
	}

	req.Header.Add("Content-Type", "application/json")

	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Errorln(err)
		}
	}(resp.Body)

	if err := processRelayResponse(resp); err != nil {
		return err
	}

	return nil
}

type RejectWithdrawalRequestInput struct {
	ID uint `json:"id"`
}

type RejectWithdrawalRequestInputWithSignature struct {
	RejectWithdrawalRequestInput
	Timestamp int64  `form:"timestamp" json:"timestamp" description:"Signature timestamp" validate:"required"`
	Signature string `form:"signature" json:"signature" description:"Signature" validate:"required"`
}

func RejectWithdrawalRequest(ctx context.Context, withdrawalRequestID uint) error {
	conf := config.GetConfig()

	input := RejectWithdrawalRequestInput{
		ID: withdrawalRequestID,
	}

	timestamp, signature, err := SignData(input, conf.Relay.Api.PrivateKey)
	if err != nil {
		return err
	}

	inputWithSignature := RejectWithdrawalRequestInputWithSignature{
		RejectWithdrawalRequestInput: input,
		Timestamp:                    timestamp,
		Signature:                    signature,
	}

	body, err := json.Marshal(inputWithSignature)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/v1/withdraw/%d/reject", conf.Relay.Api.Host, withdrawalRequestID), bytes.NewBuffer(body))
	if err != nil {
		return err
	}

	req.Header.Add("Content-Type", "application/json")

	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Errorln(err)
		}
	}(resp.Body)

	if err := processRelayResponse(resp); err != nil {
		return err
	}

	return nil
}
