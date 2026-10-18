package git

import gitcore "github.com/repogoapp/host/internal/git"

type PathParams struct {
	Path string `json:"path" doc:"The project's folder"`
}

type StatusParams struct {
	Paths []string `json:"paths" wire:"array" doc:"Projects' folders"`
}

type StatusResult struct {
	Projects []gitcore.Status `json:"projects" wire:"array"`
}

type ChangesResult struct {
	Files []gitcore.FileChange `json:"files" wire:"array"`
}

type BranchesResult struct {
	Branches []gitcore.Branch `json:"branches" wire:"array"`
}

type PatchParams struct {
	Path string `json:"path"`
	Base string `json:"base"`
	Head string `json:"head"`
	File string `json:"file"`
}

type BranchParams struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

type CreateBranchParams struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	FromRef string `json:"from_ref"`
}

type CommitPushParams struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}
