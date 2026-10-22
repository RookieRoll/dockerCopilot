package icons

import (
	"fmt"
	"net/http"

	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest/httpx"
)

func ObtainHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		iconService := svcCtx.Icons
		var err error
		if iconService == nil {
			iconService, err = newIconService()
		}
		if err != nil {
			logx.Errorf("Error initializing icon storage: %v", err)
			httpx.ErrorCtx(r.Context(), w, fmt.Errorf("failed to read config: %w", err))
			return
		}
		logos, exists, err := iconService.ImageLogos()
		if err != nil {
			logx.Errorf("Error reading config: %v", err)
			httpx.ErrorCtx(r.Context(), w, fmt.Errorf("failed to read config: %w", err))
			return
		}
		if !exists {
			logx.Info("Config file does not exist, returning empty.")
			httpx.OkJsonCtx(r.Context(), w, types.Resp{
				Code: 200,
				Msg:  "Success",
				Data: "{}",
			})
			return
		}

		logx.Infof("Total icons found: %d", len(logos))
		response := struct {
			Code int               `json:"code"`
			Msg  string            `json:"msg"`
			Data map[string]string `json:"data"`
		}{
			Code: 200,
			Msg:  "Success",
			Data: logos,
		}
		httpx.OkJsonCtx(r.Context(), w, response)
	}
}
