package types

// TaskListReq filters the persisted task query exposed by GET /api/tasks.
type TaskListReq struct {
	Status        string `form:"status,optional"`
	Type          string `form:"type,optional"`
	ContainerID   string `form:"containerID,optional"`
	ContainerName string `form:"containerName,optional"`
	Page          int    `form:"page,default=1"`
	Limit         int    `form:"limit,default=50"`
}
