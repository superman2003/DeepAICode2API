package server

import (
	"encoding/json"
	"net/http"
)

func (s *Server) handleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	if s.Updater == nil {
		writeErr(w, 503, "当前构建未启用更新器", "updater_unavailable")
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, s.Updater.Status())
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, 405, "仅支持 GET/POST", "invalid_request_error")
		return
	}
	var body struct {
		Action     string `json:"action"`
		Repository string `json:"repository"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body) != nil {
		writeErr(w, 400, "更新请求格式无效", "invalid_request_error")
		return
	}
	var err error
	switch body.Action {
	case "configure":
		err = s.Updater.SetRepository(body.Repository)
	case "check":
		_, err = s.Updater.Check(r.Context())
	case "prepare":
		err = s.Updater.Prepare()
	case "apply":
		s.maintenanceMu.Lock()
		if s.updating {
			s.maintenanceMu.Unlock()
			writeErr(w, 409, "正在升级", "updating")
			return
		}
		for _, job := range s.Registers.list() {
			if job.isRunning() {
				s.maintenanceMu.Unlock()
				writeErr(w, 409, "还有注册任务运行中，请完成后升级", "registration_running")
				return
			}
		}
		if err = s.Store.Save(); err == nil {
			err = s.Updater.Apply(len(s.Store.Accounts()))
		}
		if err == nil {
			s.updating = true
		}
		s.maintenanceMu.Unlock()
	default:
		writeErr(w, 400, "未知更新操作", "invalid_request_error")
		return
	}
	if err != nil {
		writeErr(w, 400, err.Error(), "update_error")
		return
	}
	writeJSON(w, 200, s.Updater.Status())
}
