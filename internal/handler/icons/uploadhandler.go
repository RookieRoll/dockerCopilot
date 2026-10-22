package icons

import (
	"errors"
	"net/http"

	"github.com/onlyLTY/dockerCopilot/internal/settings"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/onlyLTY/dockerCopilot/internal/types"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// multipartMemoryLimit is only the point where net/http spills file parts to
// temporary files. The actual upload limits are enforced by IconService on the
// file content and by MaxBytesReader on the whole request.
const multipartMemoryLimit = 1 << 20

func UploadHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		iconService := svcCtx.Icons
		var err error
		if iconService == nil {
			iconService, err = newIconService()
		}
		if err != nil {
			logx.Errorf("initialize icon storage: %v", err)
			writeUploadError(w, http.StatusInternalServerError, "failed to initialize icon storage")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, iconService.MaxRequestBytes())
		parseErr := r.ParseMultipartForm(multipartMemoryLimit)
		if r.MultipartForm != nil {
			defer func() {
				if cleanupErr := r.MultipartForm.RemoveAll(); cleanupErr != nil {
					logx.Errorf("remove temporary multipart upload files: %v", cleanupErr)
				}
			}()
		}
		if parseErr != nil {
			var maxBytesError *http.MaxBytesError
			if errors.As(parseErr, &maxBytesError) {
				writeUploadError(w, http.StatusBadRequest, settings.ErrIconTooLarge.Error())
				return
			}
			logx.Errorf("parse multipart icon upload: %v", parseErr)
			writeUploadError(w, http.StatusBadRequest, "failed to parse form")
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			if !errors.Is(err, http.ErrMissingFile) {
				logx.Errorf("read multipart icon file: %v", err)
			}
			writeUploadError(w, http.StatusBadRequest, "failed to get file")
			return
		}
		defer file.Close()

		filename, err := iconService.UploadIcon(r.Context(), settings.UploadIconRequest{
			ImageName: r.FormValue("imageName"),
			Filename:  header.Filename,
			Content:   file,
		})
		if err != nil {
			statusCode := http.StatusInternalServerError
			message := "failed to update config"
			switch {
			case settings.IsIconValidationError(err):
				statusCode = http.StatusBadRequest
				message = err.Error()
			case errors.Is(err, settings.ErrImageLogosInvalid):
				message = err.Error()
			}
			if !settings.IsIconValidationError(err) {
				logx.Errorf("store uploaded icon: %v", err)
			}
			writeUploadError(w, statusCode, message)
			return
		}

		httpx.OkJsonCtx(r.Context(), w, types.Resp{
			Code: 200,
			Msg:  "Success",
			Data: filename,
		})
	}
}

func newIconService() (*settings.IconService, error) {
	// imageUploadDir and imageLogosPath remain package variables so existing
	// compatibility tests can redirect all file access to a temporary directory.
	// The service obtains a process-wide lock keyed by the current mapping path.
	return settings.NewIconService(settings.IconOptions{
		ImageDir:    imageUploadDir,
		MappingPath: imageLogosPath,
	})
}

func writeUploadError(w http.ResponseWriter, statusCode int, msg string) {
	httpx.WriteJson(w, statusCode, types.Resp{
		Code: statusCode,
		Msg:  msg,
		Data: map[string]interface{}{},
	})
}
