package version

import (
	"context"
	"github.com/onlyLTY/dockerCopilot/internal/config"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/onlyLTY/dockerCopilot/internal/updater"
	"github.com/zeromicro/go-zero/core/logx"
	"os"
	"time"
)

type UpdateProgramLogic struct {
	logx.Logger
	ctx          context.Context
	updateSource config.UpdateConfig
	legacySource bool
}

func NewUpdateProgramLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateProgramLogic {
	return &UpdateProgramLogic{
		Logger:       logx.WithContext(ctx),
		ctx:          ctx,
		updateSource: svcCtx.Config.UpdateSource(),
		legacySource: svcCtx.Config.Updates.Repository == "",
	}
}

func (l *UpdateProgramLogic) UpdateProgram() (resp *types.Resp, err error) {
	resp = &types.Resp{}
	source := l.updateSource
	if l.legacySource {
		source = config.LegacyUpdateSource()
	}
	err = updater.UpdateProgramWithSource(source)
	if err != nil {
		resp.Code = 500
		resp.Msg = err.Error()
		resp.Data = map[string]interface{}{}
		return resp, err
	}
	resp.Code = 200
	resp.Msg = "success"
	go func() {
		time.Sleep(10 * time.Second)
		os.Exit(1)
	}()
	resp.Data = map[string]interface{}{}
	return resp, nil
}
