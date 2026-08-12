package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/artifact"
	"github.com/Rememorio/gofer/internal/config"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/skill"
	"github.com/Rememorio/gofer/internal/store"
	"github.com/Rememorio/gofer/internal/workspace"
)

const maxUploadFiles = 20

func (service *Service) resourceRoutes(mux *http.ServeMux) {
	service.assistantRoutes(mux)
	mux.HandleFunc("GET /api/models", service.listModels)
	mux.HandleFunc("GET /api/models/{model_name}", service.getModel)
	mux.HandleFunc("GET /api/features", service.listFeatures)
	mux.HandleFunc("GET /api/skills", service.listSkills)
	mux.HandleFunc("GET /api/skills/{skill_name}", service.getSkill)
	mux.HandleFunc("POST /api/skills/{skill_name}/enable", service.enableSkill)
	mux.HandleFunc("POST /api/skills/{skill_name}/disable", service.disableSkill)
	mux.HandleFunc("POST /api/skills/reload", service.reloadSkills)
	mux.HandleFunc("POST /api/threads/{thread_id}/uploads", service.uploadFiles)
	mux.HandleFunc("GET /api/threads/{thread_id}/uploads/limits", service.uploadLimits)
	mux.HandleFunc("GET /api/threads/{thread_id}/uploads/list", service.listUploads)
	mux.HandleFunc("DELETE /api/threads/{thread_id}/uploads/{filename}", service.deleteUpload)
	mux.HandleFunc("GET /api/threads/{thread_id}/artifacts", service.listArtifacts)
	mux.HandleFunc("GET /api/threads/{thread_id}/artifacts/{path...}", service.getArtifact)
}

type modelResource struct {
	Name             string `json:"name"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	DisplayName      string `json:"display_name"`
	SupportsThinking bool   `json:"supports_thinking"`
	SupportsVision   bool   `json:"supports_vision"`
}

func (service *Service) listModels(writer http.ResponseWriter, _ *http.Request) {
	models := make([]modelResource, len(service.config.Models))
	for index, model := range service.config.Models {
		models[index] = publicModel(model)
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"models": models, "token_usage": map[string]bool{"enabled": true}})
}

func (service *Service) getModel(writer http.ResponseWriter, request *http.Request) {
	for _, model := range service.config.Models {
		if model.Name == request.PathValue("model_name") {
			writeResourceJSON(writer, http.StatusOK, publicModel(model))
			return
		}
	}
	writeResourceError(writer, store.ErrNotFound)
}

func publicModel(model config.ModelConfig) modelResource {
	resource := modelResource{
		Name: model.Name, Provider: model.Provider, Model: model.Model,
		DisplayName: model.Name, SupportsVision: true,
	}
	if displayName, ok := model.Options["display_name"].(string); ok && strings.TrimSpace(displayName) != "" {
		resource.DisplayName = strings.TrimSpace(displayName)
	}
	if value, ok := model.Options["supports_thinking"].(bool); ok {
		resource.SupportsThinking = value
	}
	if value, ok := model.Options["supports_vision"].(bool); ok {
		resource.SupportsVision = value
	}
	return resource
}

func (service *Service) listFeatures(writer http.ResponseWriter, _ *http.Request) {
	writeResourceJSON(writer, http.StatusOK, map[string]any{
		"browser_control":      map[string]bool{"enabled": service.browser != nil},
		"web_search":           map[string]bool{"enabled": service.research != nil && service.research.HasSearch()},
		"web_fetch":            map[string]bool{"enabled": service.research != nil && service.research.HasFetch()},
		"skills":               map[string]bool{"enabled": service.skills != nil},
		"memory":               map[string]bool{"enabled": service.memories != nil},
		"scheduler":            map[string]bool{"enabled": service.config.Scheduler.Enabled},
		"read_before_write":    map[string]bool{"enabled": service.config.ReadBeforeWrite.Enabled},
		"loop_detection":       map[string]bool{"enabled": service.config.LoopDetection.Enabled},
		"tool_history_repair":  map[string]bool{"enabled": true},
		"terminal_response":    map[string]bool{"enabled": true},
		"model_length_reason":  map[string]bool{"enabled": true},
		"safety_finish_reason": map[string]bool{"enabled": true},
	})
}

func (service *Service) listSkills(writer http.ResponseWriter, _ *http.Request) {
	if service.skills == nil {
		writeResourceJSON(writer, http.StatusOK, map[string]any{"skills": []skill.Skill{}})
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"skills": service.skills.List(false)})
}

func (service *Service) getSkill(writer http.ResponseWriter, request *http.Request) {
	if service.skills == nil {
		writeResourceError(writer, skill.ErrNotFound)
		return
	}
	metadata, err := service.skills.Get(request.PathValue("skill_name"))
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	response := struct {
		skill.Skill
		Content string `json:"content,omitempty"`
	}{Skill: metadata}
	if metadata.Enabled {
		response.Content, err = service.skills.Load(request.Context(), metadata.Name)
	}
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, response)
}

func (service *Service) enableSkill(writer http.ResponseWriter, request *http.Request) {
	service.setSkillEnabled(writer, request, true)
}

func (service *Service) disableSkill(writer http.ResponseWriter, request *http.Request) {
	service.setSkillEnabled(writer, request, false)
}

func (service *Service) setSkillEnabled(writer http.ResponseWriter, request *http.Request, enabled bool) {
	if service.skills == nil {
		writeResourceError(writer, skill.ErrNotFound)
		return
	}
	service.resources.Lock()
	defer service.resources.Unlock()
	name := request.PathValue("skill_name")
	if err := service.skills.SetEnabled(request.Context(), name, enabled); err != nil {
		writeResourceError(writer, err)
		return
	}
	if err := service.skills.Project(request.Context(), service.config.Skills.ProjectionRoot); err != nil {
		_ = service.skills.SetEnabled(context.WithoutCancel(request.Context()), name, !enabled)
		writeResourceError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"success": true, "name": name, "enabled": enabled})
}

func (service *Service) reloadSkills(writer http.ResponseWriter, request *http.Request) {
	if service.skills == nil {
		writeResourceError(writer, skill.ErrNotFound)
		return
	}
	service.resources.Lock()
	defer service.resources.Unlock()
	err := service.skills.Refresh(request.Context())
	if err == nil {
		err = service.skills.Project(request.Context(), service.config.Skills.ProjectionRoot)
	}
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"success": true, "scope": "process"})
}

func (service *Service) uploadFiles(writer http.ResponseWriter, request *http.Request) {
	thread, threadWorkspace, err := service.resourceWorkspace(request)
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	defer func() { _ = threadWorkspace.Close() }()
	request.Body = http.MaxBytesReader(writer, request.Body, uploadRequestLimit(service.config.Workspace.MaxUploadBytes))
	reader, err := request.MultipartReader()
	if err != nil {
		writeResourceError(writer, errors.Join(workspace.ErrInvalidPath, err))
		return
	}
	files, err := receiveUploads(threadWorkspace, reader, maxUploadFiles)
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	for index := range files {
		files[index].ArtifactURL = artifactURL(thread.ID, files[index].VirtualPath)
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"success": true, "files": files, "message": "files uploaded"})
}

type uploadResource struct {
	Filename    string    `json:"filename"`
	Size        int64     `json:"size"`
	VirtualPath string    `json:"virtual_path"`
	ArtifactURL string    `json:"artifact_url"`
	ModifiedAt  time.Time `json:"modified_at,omitempty"`
}

func receiveUploads(threadWorkspace *workspace.Thread, reader *multipart.Reader, limit int) ([]uploadResource, error) {
	files := make([]uploadResource, 0)
	cleanup := func() {
		for _, file := range files {
			_ = threadWorkspace.RemoveUpload(file.Filename)
		}
	}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return nil, err
		}
		if part.FileName() == "" {
			_ = part.Close()
			continue
		}
		if len(files) >= limit {
			_ = part.Close()
			cleanup()
			return nil, workspace.ErrTooLarge
		}
		entry, putErr := threadWorkspace.PutUpload(part.FileName(), part)
		closeErr := part.Close()
		if putErr != nil || closeErr != nil {
			cleanup()
			return nil, errors.Join(putErr, closeErr)
		}
		files = append(files, uploadResource{Filename: entry.Name, Size: entry.Size, VirtualPath: entry.Path, ModifiedAt: entry.ModifiedAt})
	}
	if len(files) == 0 {
		return nil, workspace.ErrInvalidPath
	}
	return files, nil
}

func uploadRequestLimit(maxFileBytes int64) int64 {
	const overhead = int64(1 << 20)
	const maxInt64 = int64(^uint64(0) >> 1)
	if maxFileBytes > (maxInt64-overhead)/maxUploadFiles {
		return maxInt64
	}
	return maxFileBytes*maxUploadFiles + overhead
}

func (service *Service) uploadLimits(writer http.ResponseWriter, request *http.Request) {
	_, threadWorkspace, err := service.resourceWorkspace(request)
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	_ = threadWorkspace.Close()
	writeResourceJSON(writer, http.StatusOK, map[string]any{"max_file_size": service.config.Workspace.MaxUploadBytes, "max_files": maxUploadFiles, "max_total_size": service.config.Workspace.MaxUploadBytes * maxUploadFiles})
}

func (service *Service) listUploads(writer http.ResponseWriter, request *http.Request) {
	thread, threadWorkspace, err := service.resourceWorkspace(request)
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	defer func() { _ = threadWorkspace.Close() }()
	listed, err := threadWorkspace.List(workspace.UploadsRoot, workspace.ListOptions{MaxDepth: 1, MaxResults: 1000})
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	files := make([]uploadResource, 0, len(listed.Entries))
	for _, entry := range listed.Entries {
		if !entry.Directory {
			files = append(files, uploadResource{Filename: entry.Name, Size: entry.Size, VirtualPath: entry.Path, ArtifactURL: artifactURL(thread.ID, entry.Path), ModifiedAt: entry.ModifiedAt})
		}
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"files": files, "count": len(files), "truncated": listed.Truncated})
}

func (service *Service) deleteUpload(writer http.ResponseWriter, request *http.Request) {
	_, threadWorkspace, err := service.resourceWorkspace(request)
	if err == nil {
		defer func() { _ = threadWorkspace.Close() }()
		err = threadWorkspace.RemoveUpload(request.PathValue("filename"))
	}
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"success": true})
}

func (service *Service) listArtifacts(writer http.ResponseWriter, request *http.Request) {
	_, threadWorkspace, err := service.resourceWorkspace(request)
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	defer func() { _ = threadWorkspace.Close() }()
	listed, err := threadWorkspace.List(workspace.OutputsRoot, workspace.ListOptions{MaxDepth: 20, MaxResults: 1000})
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	artifacts := make([]artifact.Artifact, 0, len(listed.Entries))
	for _, entry := range listed.Entries {
		if entry.Directory || threadWorkspace.IsInternalOutputPath(entry.Path) {
			continue
		}
		metadata, inspectErr := artifact.Inspect(threadWorkspace, entry.Path)
		if inspectErr != nil {
			writeResourceError(writer, inspectErr)
			return
		}
		artifacts = append(artifacts, metadata)
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"artifacts": artifacts, "truncated": listed.Truncated})
}

func (service *Service) getArtifact(writer http.ResponseWriter, request *http.Request) {
	_, threadWorkspace, err := service.resourceWorkspace(request)
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	defer func() { _ = threadWorkspace.Close() }()
	virtualPath := "/" + strings.TrimPrefix(path.Clean("/"+request.PathValue("path")), "/")
	reader, metadata, err := artifact.OpenFile(threadWorkspace, virtualPath)
	if err != nil {
		writeResourceError(writer, err)
		return
	}
	defer func() { _ = reader.Close() }()
	disposition := "inline"
	if request.URL.Query().Get("download") == "true" || activeContent(metadata.MediaType) {
		disposition = "attachment"
	}
	writer.Header().Set("Content-Type", metadata.MediaType)
	writer.Header().Set("Content-Disposition", disposition+"; filename*=UTF-8''"+url.PathEscape(metadata.Name))
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if seeker, ok := reader.(io.ReadSeeker); ok {
		http.ServeContent(writer, request, metadata.Name, metadata.ModifiedAt, seeker)
		return
	}
	writer.Header().Set("Content-Length", strconv.FormatInt(metadata.Size, 10))
	_, _ = io.Copy(writer, reader)
}

func activeContent(mediaType string) bool {
	mediaType = strings.ToLower(strings.Split(mediaType, ";")[0])
	return mediaType == "text/html" || mediaType == "application/xhtml+xml" || mediaType == "image/svg+xml"
}

func (service *Service) resourceWorkspace(request *http.Request) (domain.Thread, *workspace.Thread, error) {
	thread, err := service.ownedThread(request)
	if err != nil {
		return domain.Thread{}, nil, err
	}
	threadWorkspace, err := service.workspaces.Open(thread.ID)
	return thread, threadWorkspace, err
}

func (service *Service) ownedThread(request *http.Request) (domain.Thread, error) {
	threadID, err := domain.ParseThreadID(request.PathValue("thread_id"))
	if err != nil {
		return domain.Thread{}, err
	}
	thread, err := service.store.Thread(request.Context(), threadID)
	if err != nil || !store.ThreadOwnedBy(thread, requestUser(request.Context())) {
		if err == nil {
			err = store.ErrNotFound
		}
		return domain.Thread{}, err
	}
	return thread, nil
}

func artifactURL(threadID domain.ThreadID, virtualPath string) string {
	return "/api/threads/" + string(threadID) + "/artifacts/" + strings.TrimPrefix(virtualPath, "/")
}

func writeResourceError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, skill.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, workspace.ErrTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, workspace.ErrInvalidPath), errors.Is(err, workspace.ErrNotRegular), errors.Is(err, artifact.ErrInvalidArtifact):
		status = http.StatusBadRequest
	case errors.Is(err, store.ErrInvalidQuery):
		status = http.StatusBadRequest
	}
	writeResourceJSON(writer, status, map[string]string{"error": http.StatusText(status)})
}

func writeResourceJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
