package types

// ContainerBatchReq is the JSON contract for asynchronous multi-container
// operations. A nil DeleteOldContainer preserves the configured default.
type ContainerBatchReq struct {
	Action             string   `json:"action"`
	ContainerIDs       []string `json:"containerIDs"`
	DeleteOldContainer *bool    `json:"deleteOldContainer,optional"`
}

// ImageCleanupReq is the JSON contract for asynchronous image cleanup.
type ImageCleanupReq struct {
	Policy string `json:"policy"`
}
