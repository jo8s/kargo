package bitbucketserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/go-cleanhttp"

	"github.com/akuity/kargo/pkg/gitprovider"
	"github.com/akuity/kargo/pkg/urls"
)

const (
	// ProviderName distinguishes this from the Bitbucket Cloud provider.
	ProviderName = "bitbucket-server"

	// Bitbucket Server API 1.0 PR States
	prStateOpen   = "OPEN"
	prStateMerged = "MERGED"
)

var registration = gitprovider.Registration{
	Predicate: func(repoURL string) bool {
		u, err := url.Parse(repoURL)
		if err != nil {
			return false
		}
		host := strings.ToLower(u.Hostname())
		// Match internal bitbucket hosts, excluding the public bitbucket.org
		return strings.Contains(host, "bitbucket") && host != "bitbucket.org"
	},
	NewProvider: func(
		repoURL string,
		opts *gitprovider.Options,
	) (gitprovider.Interface, error) {
		return NewProvider(repoURL, opts)
	},
}

func init() {
	gitprovider.Register(ProviderName, registration)
}

type bitbucketPR struct {
	ID          int64  `json:"id"`
	Version     int    `json:"version"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	CreatedDate int64  `json:"createdDate"`
	FromRef     struct {
		ID           string `json:"id"`
		LatestCommit string `json:"latestCommit"`
	} `json:"fromRef"`
	ToRef struct {
		ID           string `json:"id"`
		LatestCommit string `json:"latestCommit"`
	} `json:"toRef"`
	Links struct {
		Self []struct {
			Href string `json:"href"`
		} `json:"self"`
	} `json:"links"`
}

type provider struct {
	apiBaseURL string
	token      string
	httpClient *http.Client
}

func NewProvider(repoURL string, opts *gitprovider.Options) (gitprovider.Interface, error) {
	if opts == nil {
		opts = &gitprovider.Options{}
	}

	u, err := url.Parse(urls.NormalizeGit(repoURL))
	if err != nil {
		return nil, fmt.Errorf("parse repo URL: %w", err)
	}

	// Typical Bitbucket Server path: /projects/PROJ/repos/REPO
	path := strings.Trim(u.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 4 {
		return nil, fmt.Errorf("invalid Bitbucket Server URL format: %s", u.Path)
	}

	projectType := parts[0] // "projects" or "users"
	project := parts[1]
	repoSlug := parts[3]

	return &provider{
		apiBaseURL: fmt.Sprintf("%s://%s/rest/api/1.0/%s/%s/repos/%s",
			u.Scheme, u.Host, projectType, project, repoSlug),
		token:      opts.Token,
		httpClient: cleanhttp.DefaultClient(),
	}, nil
}

func (p *provider) CreatePullRequest(
	ctx context.Context,
	opts *gitprovider.CreatePullRequestOpts,
) (*gitprovider.PullRequest, error) {
	apiURL := fmt.Sprintf("%s/pull-requests", p.apiBaseURL)

	payload := map[string]any{
		"title":       opts.Title,
		"description": opts.Description,
		"fromRef":     map[string]string{"id": "refs/heads/" + opts.Head},
		"toRef":       map[string]string{"id": "refs/heads/" + opts.Base},
	}

	resp, err := p.doRequest(ctx, http.MethodPost, apiURL, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var res bitbucketPR
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return p.toProviderPR(&res), nil
}

func (p *provider) GetPullRequest(ctx context.Context, id int64) (*gitprovider.PullRequest, error) {
	apiURL := fmt.Sprintf("%s/pull-requests/%d", p.apiBaseURL, id)
	resp, err := p.doRequest(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var res bitbucketPR
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return p.toProviderPR(&res), nil
}

func (p *provider) ListPullRequests(ctx context.Context, opts *gitprovider.ListPullRequestOptions,) ([]gitprovider.PullRequest, error) {
	state := "OPEN"
	if opts != nil && opts.State == gitprovider.PullRequestStateClosed {
		state = "MERGED"
	}

	apiURL := fmt.Sprintf("%s/pull-requests?state=%s", p.apiBaseURL, state)
	resp, err := p.doRequest(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Values []bitbucketPR `json:"values"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	prs := make([]gitprovider.PullRequest, 0, len(result.Values))
	for _, v := range result.Values {
		// 1. Filter by Head Branch
		if opts != nil && opts.HeadBranch != "" {
			if !strings.HasSuffix(v.FromRef.ID, "/"+opts.HeadBranch) && v.FromRef.ID != opts.HeadBranch {
				continue
			}
		}

		// 2. Filter by Base Branch (ToRef)
		if opts != nil && opts.BaseBranch != "" {
			if !strings.HasSuffix(v.ToRef.ID, "/"+opts.BaseBranch) && v.ToRef.ID != opts.BaseBranch {
				continue
			}
		}

		// 3. Filter by Head Commit SHA
		if opts != nil && opts.HeadCommit != "" {
			if v.FromRef.LatestCommit != opts.HeadCommit {
				continue
			}
		}

		// If it passed all filters, add it to our list
		prs = append(prs, *p.toProviderPR(&v))
	}

	return prs, nil
}

func (p *provider) MergePullRequest(ctx context.Context, id int64, _ *gitprovider.MergePullRequestOpts,) (*gitprovider.PullRequest, bool, error) {
	// 1. Get current PR state to retrieve the 'version' field (required by Bitbucket Server)
	pr, err := p.GetPullRequest(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if pr.Merged {
		return pr, true, nil
	}

	raw, ok := pr.Object.(*bitbucketPR)
	if !ok {
		return nil, false, fmt.Errorf("unexpected object type: %T", pr.Object)
	}

	// 2. Perform merge using version query parameter
	apiURL := fmt.Sprintf("%s/pull-requests/%d/merge?version=%d", p.apiBaseURL, id, raw.Version)

	resp, err := p.doRequest(ctx, http.MethodPost, apiURL, nil)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	var merged bitbucketPR
	if err := json.NewDecoder(resp.Body).Decode(&merged); err != nil {
		return nil, false, err
	}
	return p.toProviderPR(&merged), true, nil
}

func (p *provider) GetCommitURL(repoURL string, sha string) (string, error) {
	u, _ := url.Parse(urls.NormalizeGit(repoURL))
	path := strings.TrimSuffix(u.Path, ".git")
	return fmt.Sprintf("%s://%s%s/commits/%s", u.Scheme, u.Host, path, sha), nil
}

// doRequest is a helper to handle HTTP headers and Bearer Token auth
func (p *provider) doRequest(ctx context.Context, method, apiURL string, body any) (*http.Response, error) {
	var buf io.ReadWriter
	if body != nil {
		buf = new(bytes.Buffer)
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, apiURL, buf)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}

	// #nosec G704
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("bitbucket server api error: %s", resp.Status)
	}
	return resp, nil
}

func (p *provider) toProviderPR(bb *bitbucketPR) *gitprovider.PullRequest {
	link := ""
	if len(bb.Links.Self) > 0 {
		link = bb.Links.Self[0].Href
	}
	createdAt := time.Unix(0, bb.CreatedDate*int64(time.Millisecond))

	return &gitprovider.PullRequest{
		Number:    bb.ID,
		URL:       link,
		Open:      bb.State == prStateOpen,
		Merged:    bb.State == prStateMerged,
		HeadSHA:   bb.FromRef.LatestCommit,
		CreatedAt: &createdAt,
		Object:    bb, // Pass the raw struct for stateful merge operations
	}
}
