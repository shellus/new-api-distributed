package controller

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	edgeservice "github.com/QuantumNous/new-api/service/edge"

	"github.com/gin-gonic/gin"
)

func CreateEdgeNode(c *gin.Context) {
	var request dto.EdgeNodeCreateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiError(c, err)
		return
	}
	response, err := edgeservice.CreateNode(request)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, response)
}

func ListEdgeNodes(c *gin.Context) {
	nodes, err := edgeservice.ListNodes()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nodes)
}

func PublishEdgeSnapshot(c *gin.Context) {
	manifest, err := edgeservice.CompileAndPublishEdgeSnapshot()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, manifest)
}

func GetLatestEdgeSnapshot(c *gin.Context) {
	manifest, err := model.GetLatestPublishedEdgeCompiledSnapshotManifest(0)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, manifest)
}

func UpdateEdgeNodeStatus(c *gin.Context) {
	nodeID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || nodeID <= 0 {
		common.ApiErrorMsg(c, "invalid edge node ID")
		return
	}
	var request dto.EdgeNodeStatusUpdateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiError(c, err)
		return
	}
	response, err := edgeservice.UpdateNodeStatus(nodeID, model.EdgeNodeStatus(request.Status))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, response)
}

func ClearEdgeNodeSettlementCircuit(c *gin.Context) {
	nodeID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || nodeID <= 0 {
		common.ApiErrorMsg(c, "invalid edge node ID")
		return
	}
	response, err := edgeservice.ClearNodeSettlementCircuit(nodeID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, response)
}

func RotateEdgeNodeCredential(c *gin.Context) {
	nodeID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || nodeID <= 0 {
		common.ApiErrorMsg(c, "invalid edge node ID")
		return
	}
	var request dto.EdgeNodeCredentialRotateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiError(c, err)
		return
	}
	response, err := edgeservice.RotateNodeCredential(nodeID, request)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, response)
}
