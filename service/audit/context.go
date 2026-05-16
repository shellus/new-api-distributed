package audit

import "github.com/gin-gonic/gin"

const (
	contextKeyModelInfo   = "audit_model_info"
	contextKeyBillingInfo = "audit_billing_info"
)

func SetModelInfo(c *gin.Context, info ModelInfo) {
	if c == nil {
		return
	}
	c.Set(contextKeyModelInfo, info)
}

func ModelInfoFromContext(c *gin.Context) ModelInfo {
	if c == nil {
		return ModelInfo{}
	}
	value, exists := c.Get(contextKeyModelInfo)
	if !exists {
		return ModelInfo{}
	}
	info, _ := value.(ModelInfo)
	return info
}

func SetBillingInfo(c *gin.Context, info BillingInfo) {
	if c == nil {
		return
	}
	c.Set(contextKeyBillingInfo, info)
}

func BillingInfoFromContext(c *gin.Context) BillingInfo {
	if c == nil {
		return BillingInfo{}
	}
	value, exists := c.Get(contextKeyBillingInfo)
	if !exists {
		return BillingInfo{}
	}
	info, _ := value.(BillingInfo)
	return info
}
