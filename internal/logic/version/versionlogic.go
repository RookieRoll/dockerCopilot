package version

import (
	"context"
	"net/http"

	"github.com/onlyLTY/dockerCopilot/internal/config"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/onlyLTY/dockerCopilot/internal/updater"
	"github.com/zeromicro/go-zero/core/logx"
)

type VersionLogic struct {
	logx.Logger
	ctx          context.Context
	updateSource config.UpdateConfig
	legacySource bool
}

type invalidVersionRequestError string

func (e invalidVersionRequestError) Error() string     { return string(e) }
func (e invalidVersionRequestError) HTTPStatus() int   { return http.StatusBadRequest }
func (e invalidVersionRequestError) ErrorCode() string { return "INVALID_VERSION_REQUEST" }

func NewVersionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VersionLogic {
	return &VersionLogic{
		Logger:       logx.WithContext(ctx),
		ctx:          ctx,
		updateSource: svcCtx.Config.UpdateSource(),
		legacySource: svcCtx.Config.Updates.Repository == "",
	}
}

func (l *VersionLogic) Version(req *types.VersionReq) (resp *types.Resp, err error) {
	resp = &types.Resp{}
	if req.Type == "local" {
		resp.Code = 200
		resp.Msg = "success"
		resp.Data = map[string]string{
			"version":   config.Version,
			"buildDate": config.BuildDate,
		}
		return resp, nil
	} else if req.Type == "remote" {
		updateSource := l.updateSource
		if l.legacySource {
			updateSource = config.LegacyUpdateSource()
		}
		remoteVersion, err := updater.CheckRemoteVersion(l.ctx, updateSource)
		if err != nil {
			resp.Code = 50001
			resp.Msg = "获取版本错误" + err.Error()
			resp.Data = map[string]string{
				"remoteVersion": config.Version,
			}
			return resp, err
		} else if remoteVersion != config.Version {
			resp.Code = 200
			resp.Msg = "程序有更新"
			resp.Data = map[string]string{
				"remoteVersion": remoteVersion,
			}
			return resp, nil
		} else {
			resp.Code = 200
			resp.Msg = "程序无更新"
			resp.Data = map[string]string{
				"remoteVersion": remoteVersion,
			}
			return resp, nil
		}

	} else {
		resp.Code = 400
		resp.Msg = "type 参数错误"
		resp.Data = map[string]string{}
		return resp, invalidVersionRequestError("type 参数错误")
	}
}
