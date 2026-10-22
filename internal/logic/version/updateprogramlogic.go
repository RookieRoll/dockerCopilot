package version

import (
	"context"

	"github.com/google/uuid"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/observability"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateProgramLogic struct {
	logx.Logger
	ctx                    context.Context
	serviceContext         *svc.ServiceContext
	updateSource           config.UpdateConfig
	requestID              string
	validateProgramHandoff func(context.Context) error
}

func NewUpdateProgramLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateProgramLogic {
	return &UpdateProgramLogic{
		Logger:         logx.WithContext(ctx),
		ctx:            ctx,
		serviceContext: svcCtx,
		updateSource:   svcCtx.Config.UpdateSource(),
		requestID:      observability.RequestIDFromContext(ctx),
	}
}

// SetRequestID associates the client-visible request identifier with the
// durable task record created by this request.
func (l *UpdateProgramLogic) SetRequestID(requestID string) {
	if l != nil && uuid.Validate(requestID) == nil {
		l.requestID = requestID
	}
}

func (l *UpdateProgramLogic) UpdateProgram() (resp *types.Resp, err error) {
	return l.submitProgramUpdate()
}
