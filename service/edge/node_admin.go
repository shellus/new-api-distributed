package edge

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgeauth"

	"gorm.io/gorm"
)

const defaultEdgeCredentialLifetime = 365 * 24 * time.Hour

// CreateNode provisions one master-side node identity and returns the
// corresponding private key exactly once. Only the Ed25519 public key is
// written to the master database.
func CreateNode(request dto.EdgeNodeCreateRequest) (*dto.EdgeNodeCreateResponse, error) {
	request.NodeID = strings.TrimSpace(request.NodeID)
	request.Name = strings.TrimSpace(request.Name)
	request.Region = strings.TrimSpace(request.Region)
	if err := edgeauth.ValidateNodeID(request.NodeID); err != nil {
		return nil, err
	}
	if request.Name == "" {
		return nil, errors.New("edge node name is empty")
	}
	if request.Generation == 0 {
		request.Generation = 1
	}
	if request.Generation < 1 {
		return nil, errors.New("edge node generation must be greater than zero")
	}
	if request.MaxOutstandingQuota <= 0 {
		return nil, errors.New("edge node max outstanding quota must be greater than zero")
	}

	now := time.Now()
	expiresAt := now.Add(defaultEdgeCredentialLifetime).Unix()
	if request.CredentialExpiresAtUnixMilli > 0 {
		expiresAt = request.CredentialExpiresAtUnixMilli / int64(time.Second/time.Millisecond)
		if expiresAt <= now.Unix() {
			return nil, errors.New("edge node credential expiry must be in the future")
		}
	}

	node := &model.EdgeNode{
		NodeUID:             request.NodeID,
		Name:                request.Name,
		Region:              request.Region,
		Status:              model.EdgeNodeStatusActive,
		Generation:          request.Generation,
		ProtocolVersion:     dto.EdgeControlProtocolVersionV2,
		MaxOutstandingQuota: request.MaxOutstandingQuota,
	}
	credential, privateMaterial, err := provisionNodeCredential(request.Generation, now.Unix(), expiresAt)
	if err != nil {
		return nil, err
	}

	err = model.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(node).Error; err != nil {
			return err
		}
		credential.NodeID = node.ID
		return tx.Create(credential).Error
	})
	if err != nil {
		return nil, fmt.Errorf("create edge node: %w", err)
	}

	return &dto.EdgeNodeCreateResponse{
		Node:       edgeNodeAdminView(node),
		Credential: provisionedCredentialView(credential, privateMaterial),
	}, nil
}

func ListNodes() ([]dto.EdgeNodeAdminView, error) {
	var nodes []model.EdgeNode
	if err := model.DB.Order("id asc").Find(&nodes).Error; err != nil {
		return nil, err
	}
	views := make([]dto.EdgeNodeAdminView, 0, len(nodes))
	for i := range nodes {
		views = append(views, edgeNodeAdminView(&nodes[i]))
	}
	return views, nil
}

func UpdateNodeStatus(nodeID int64, status model.EdgeNodeStatus) (*dto.EdgeNodeAdminView, error) {
	if !status.Valid() {
		return nil, model.ErrInvalidEdgeNodeStatus
	}
	var updated *model.EdgeNode
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		node, err := model.LockEdgeNodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		if node.Status == model.EdgeNodeStatusRevoked && status != model.EdgeNodeStatusRevoked {
			return errors.New("revoked edge node cannot be reactivated")
		}
		if node.Status != status {
			now := time.Now().Unix()
			if err := tx.Model(&model.EdgeNode{}).Where("id = ?", node.ID).Updates(map[string]any{
				"status":     status,
				"updated_at": now,
			}).Error; err != nil {
				return err
			}
			if status == model.EdgeNodeStatusRevoked {
				if err := tx.Model(&model.EdgeNodeCredential{}).
					Where("node_id = ? AND status <> ?", node.ID, model.EdgeNodeCredentialStatusRevoked).
					Updates(map[string]any{
						"status":     model.EdgeNodeCredentialStatusRevoked,
						"revoked_at": now,
						"updated_at": now,
					}).Error; err != nil {
					return err
				}
			}
			node.Status = status
			node.UpdatedAt = now
		}
		updated = node
		return nil
	})
	if err != nil {
		return nil, err
	}
	view := edgeNodeAdminView(updated)
	return &view, nil
}

func RotateNodeCredential(nodeID int64, request dto.EdgeNodeCredentialRotateRequest) (*dto.EdgeNodeProvisionedCredential, error) {
	now := time.Now()
	expiresAt := now.Add(defaultEdgeCredentialLifetime).Unix()
	if request.ExpiresAtUnixMilli > 0 {
		expiresAt = request.ExpiresAtUnixMilli / int64(time.Second/time.Millisecond)
		if expiresAt <= now.Unix() {
			return nil, errors.New("edge node credential expiry must be in the future")
		}
	}

	var credential *model.EdgeNodeCredential
	var privateMaterial string
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		node, err := model.LockEdgeNodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		if node.Status == model.EdgeNodeStatusRevoked {
			return ErrControlNodeRevoked
		}
		credential, privateMaterial, err = provisionNodeCredential(node.Generation, now.Unix(), expiresAt)
		if err != nil {
			return err
		}
		credential.NodeID = node.ID
		if err := tx.Model(&model.EdgeNodeCredential{}).
			Where("node_id = ? AND generation = ? AND status = ?", node.ID, node.Generation, model.EdgeNodeCredentialStatusActive).
			Updates(map[string]any{
				"status":     model.EdgeNodeCredentialStatusRetired,
				"updated_at": now.Unix(),
			}).Error; err != nil {
			return err
		}
		return tx.Create(credential).Error
	})
	if err != nil {
		return nil, err
	}
	view := provisionedCredentialView(credential, privateMaterial)
	return &view, nil
}

func provisionNodeCredential(generation int64, notBefore int64, expiresAt int64) (*model.EdgeNodeCredential, string, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate edge node credential: %w", err)
	}
	publicMaterial, err := edgeauth.EncodePublicKey(publicKey)
	if err != nil {
		return nil, "", err
	}
	privateMaterial, err := edgeauth.EncodePrivateKey(privateKey)
	if err != nil {
		return nil, "", err
	}
	credentialRandom := make([]byte, 16)
	if _, err := rand.Read(credentialRandom); err != nil {
		return nil, "", fmt.Errorf("generate edge credential ID: %w", err)
	}
	return &model.EdgeNodeCredential{
		CredentialUID:  "edge-key-" + hex.EncodeToString(credentialRandom),
		Generation:     generation,
		Algorithm:      edgeauth.Algorithm,
		VerifyMaterial: publicMaterial,
		Status:         model.EdgeNodeCredentialStatusActive,
		NotBefore:      notBefore,
		ExpiresAt:      expiresAt,
	}, privateMaterial, nil
}

func provisionedCredentialView(credential *model.EdgeNodeCredential, privateMaterial string) dto.EdgeNodeProvisionedCredential {
	if credential == nil {
		return dto.EdgeNodeProvisionedCredential{}
	}
	return dto.EdgeNodeProvisionedCredential{
		CredentialID:       credential.CredentialUID,
		Algorithm:          credential.Algorithm,
		PrivateKey:         privateMaterial,
		Fingerprint:        credential.Fingerprint,
		NotBeforeUnixMilli: time.Unix(credential.NotBefore, 0).UnixMilli(),
		ExpiresAtUnixMilli: time.Unix(credential.ExpiresAt, 0).UnixMilli(),
		ReturnedOnce:       true,
	}
}

func edgeNodeAdminView(node *model.EdgeNode) dto.EdgeNodeAdminView {
	if node == nil {
		return dto.EdgeNodeAdminView{}
	}
	return dto.EdgeNodeAdminView{
		ID:                  node.ID,
		NodeID:              node.NodeUID,
		Name:                node.Name,
		Region:              node.Region,
		Status:              string(node.Status),
		Generation:          node.Generation,
		ProtocolVersion:     node.ProtocolVersion,
		DeclaredPublicURL:   node.DeclaredPublicURL,
		SoftwareVersion:     node.SoftwareVersion,
		MaxOutstandingQuota: node.MaxOutstandingQuota,
		LastSeenAtUnixMilli: time.Unix(node.LastSeenAt, 0).UnixMilli(),
		CreatedAtUnixMilli:  time.Unix(node.CreatedAt, 0).UnixMilli(),
		UpdatedAtUnixMilli:  time.Unix(node.UpdatedAt, 0).UnixMilli(),
	}
}
