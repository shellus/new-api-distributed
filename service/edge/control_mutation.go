package edge

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"gorm.io/gorm"
)

var ErrControlReceiptProcessing = errors.New("edge control request receipt is still processing")

var errRollbackControlRejection = errors.New("roll back rejected edge control mutation")

type ControlHTTPResponse struct {
	StatusCode int
	Body       []byte
	Replayed   bool
}

type ControlMutationResult struct {
	StatusCode int
	ResultRef  string
	Response   any
}

type ControlMutation func(tx *gorm.DB, identity *model.EdgeControlIdentity) (*ControlMutationResult, error)

// ExecuteControlMutation owns replay protection, identity revalidation and
// exact response persistence around one trusted control-plane mutation.
func ExecuteControlMutation(principal *ControlPrincipal, requestKind string, receiptTTL time.Duration, mutate ControlMutation) (*ControlHTTPResponse, error) {
	if principal == nil || principal.SignedRequest == nil {
		return nil, errors.New("edge control principal is missing")
	}
	if receiptTTL <= 0 {
		return nil, errors.New("edge control receipt TTL must be positive")
	}
	if mutate == nil {
		return nil, errors.New("edge control mutation is missing")
	}

	var response *ControlHTTPResponse
	processingReplay := false
	now := time.Now()
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		identity, err := model.LockEdgeControlIdentityTx(tx, principal.NodeUID, principal.Generation, principal.CredentialUID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrControlAuthentication
			}
			return err
		}
		if identity.Node.ID != principal.NodeID ||
			identity.Credential.ID != principal.CredentialID ||
			identity.Credential.Fingerprint != principal.CredentialFingerprint {
			return ErrControlAuthentication
		}
		if identity.Node.Status == model.EdgeNodeStatusRevoked {
			return ErrControlNodeRevoked
		}
		if identity.Node.ProtocolVersion != dto.EdgeControlProtocolVersionV1 {
			return ErrControlProtocol
		}
		if err := identity.Credential.ValidateAt(now.Unix()); err != nil {
			return fmt.Errorf("%w: credential changed after authentication", ErrControlAuthentication)
		}

		claim, err := model.ClaimEdgeRequestReceiptTx(tx, &model.EdgeRequestReceipt{
			NodeID:         principal.NodeID,
			CredentialID:   principal.CredentialID,
			Generation:     principal.Generation,
			NonceHash:      principal.NonceHash,
			RequestKind:    requestKind,
			RequestHash:    principal.RequestHash,
			IdempotencyKey: principal.SignedRequest.Metadata.IdempotencyKey,
			ExpiresAt:      now.Add(receiptTTL).Unix(),
		})
		if err != nil {
			return err
		}
		if err := tx.Model(&model.EdgeNodeCredential{}).
			Where("id = ? AND status = ?", identity.Credential.ID, model.EdgeNodeCredentialStatusActive).
			Updates(map[string]any{
				"last_used_at": now.Unix(),
				"updated_at":   now.Unix(),
			}).Error; err != nil {
			return err
		}
		if !claim.Claimed {
			switch claim.Receipt.Status {
			case model.EdgeRequestReceiptStatusCommitted, model.EdgeRequestReceiptStatusRejected:
				response = &ControlHTTPResponse{
					StatusCode: claim.Receipt.ResponseStatus,
					Body:       []byte(claim.Receipt.ResponsePayload),
					Replayed:   true,
				}
				return nil
			case model.EdgeRequestReceiptStatusProcessing:
				processingReplay = true
				return nil
			default:
				return errors.New("edge control receipt has an invalid status")
			}
		}

		var result *ControlMutationResult
		mutationErr := tx.Transaction(func(mutationTx *gorm.DB) error {
			var err error
			result, err = mutate(mutationTx, identity)
			if err != nil {
				return err
			}
			if result == nil {
				return errors.New("edge control mutation returned no result")
			}
			if result.StatusCode < 200 || result.StatusCode > 599 {
				return errors.New("edge control mutation returned an unsupported HTTP status")
			}
			if result.StatusCode >= 300 && result.StatusCode < 400 {
				return errors.New("edge control mutation cannot persist a 3xx response")
			}
			if result.StatusCode >= 400 {
				// A trusted domain rejection must keep its durable receipt, but no
				// authoritative state touched while deriving that rejection may
				// escape the savepoint. This is especially important for accounting
				// functions that discover a conflict after creating intermediate rows.
				return errRollbackControlRejection
			}
			return nil
		})
		if mutationErr != nil && !errors.Is(mutationErr, errRollbackControlRejection) {
			return mutationErr
		}
		payload, err := common.Marshal(result.Response)
		if err != nil {
			return err
		}
		if result.StatusCode >= 200 && result.StatusCode <= 299 {
			_, err = model.CommitEdgeRequestReceiptTx(tx, claim.Receipt.ID, result.ResultRef, result.StatusCode, json.RawMessage(payload))
		} else if result.StatusCode >= 400 {
			_, err = model.RejectEdgeRequestReceiptTx(tx, claim.Receipt.ID, result.StatusCode, json.RawMessage(payload))
		}
		if err != nil {
			return err
		}
		response = &ControlHTTPResponse{
			StatusCode: result.StatusCode,
			Body:       payload,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if processingReplay {
		return nil, ErrControlReceiptProcessing
	}
	if response == nil {
		return nil, errors.New("edge control mutation produced no response")
	}
	return response, nil
}
