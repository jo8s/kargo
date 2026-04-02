package bitbucketserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/akuity/kargo/pkg/gitprovider"
)

func TestRegistration(t *testing.T) {
	testCases := []struct {
		name     string
		repoURL  string
		expected bool
	}{
		{
			name:     "internal bitbucket server",
			repoURL:  "https://bitbucket.mycompany.nl/projects/proj/repos/repo",
			expected: true,
		},
		{
			name:     "bitbucket cloud should fail",
			repoURL:  "https://bitbucket.org/user/repo",
			expected: false,
		},
		{
			name:     "github should fail",
			repoURL:  "https://github.com/user/repo",
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, registration.Predicate(tc.repoURL))
		})
	}
}

func TestNewProvider(t *testing.T) {
	testCases := []struct {
		name            string
		repoURL         string
		expectedBaseURL string
	}{
		{
			name:            "standard project repo",
			repoURL:         "https://bitbucket.test/projects/proj/repos/my-repo",
			expectedBaseURL: "https://bitbucket.test/rest/api/1.0/projects/proj/repos/my-repo",
		},
		{
			name:            "personal user repo",
			repoURL:         "https://bitbucket.test/users/user/repos/kargo",
			expectedBaseURL: "https://bitbucket.test/rest/api/1.0/users/user/repos/kargo",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pInterface, err := NewProvider(tc.repoURL, &gitprovider.Options{Token: "secret"})
			require.NoError(t, err)

			p, ok := pInterface.(*provider)
			require.True(t, ok)
			assert.Equal(t, tc.expectedBaseURL, p.apiBaseURL)
			assert.Equal(t, "secret", p.token)
		})
	}
}

func TestCreatePullRequest(t *testing.T) {
	t.Run("successful creation with full response validation", func(t *testing.T) {
		// Prepare a fixed time for testing
		now := time.Now().Truncate(time.Second).UTC()
		millis := now.UnixNano() / int64(time.Millisecond)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 1. Validate the Request
			assert.Equal(t, http.MethodPost, r.Method)

			var reqBody map[string]any
			err := json.NewDecoder(r.Body).Decode(&reqBody)
			require.NoError(t, err)
			assert.Equal(t, "Kargo PR", reqBody["title"])

			// 2. Mock the Bitbucket Server Response
			resp := map[string]any{
				"id":          int64(100),
				"version":     1,
				"title":       "Kargo PR",
				"state":       "OPEN",
				"createdDate": millis,
				"fromRef": map[string]any{
					"latestCommit": "sha-new-pr",
				},
				"links": map[string]any{
					"self": []map[string]any{
						{"href": "https://bitbucket.test/pr/100"},
					},
				},
			}

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		repoURL := fmt.Sprintf("%s/projects/proj/repos/repo", server.URL)
		p, _ := NewProvider(repoURL, &gitprovider.Options{Token: "test-token"})

		pr, err := p.CreatePullRequest(context.Background(), &gitprovider.CreatePullRequestOpts{
			Title: "Kargo PR",
			Head:  "feature",
			Base:  "main",
		})

		// 3. Assertions on the returned PullRequest object
		require.NoError(t, err)
		require.NotNil(t, pr)
		assert.Equal(t, int64(100), pr.Number)
		assert.Equal(t, "sha-new-pr", pr.HeadSHA)
		assert.Equal(t, "https://bitbucket.test/pr/100", pr.URL)

		// Time validation
		require.NotNil(t, pr.CreatedAt)
		assert.Equal(t, now.Unix(), pr.CreatedAt.Unix())
	})
}

func TestGetPullRequest(t *testing.T) {
	t.Run("successful retrieval with full metadata", func(t *testing.T) {
		now := time.Now().Truncate(time.Second)
		millis := now.UnixNano() / int64(time.Millisecond)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			resp := map[string]any{
				"id":          int64(1),
				"version":     1,
				"title":       "Kargo Promotion",
				"description": "Automated PR",
				"state":       "OPEN",
				"createdDate": millis,
				"fromRef": map[string]any{
					"latestCommit": "abcdef1234567890",
				},
				"links": map[string]any{
					"self": []map[string]any{
						{"href": "https://bitbucket.test/pr/1"},
					},
				},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		repoURL := fmt.Sprintf("%s/projects/proj/repos/repo", server.URL)
		pInterface, _ := NewProvider(repoURL, &gitprovider.Options{})
		p, ok := pInterface.(*provider)
		require.True(t, ok)

		pr, err := p.GetPullRequest(context.Background(), 1)

		require.NoError(t, err)
		assert.Equal(t, int64(1), pr.Number)
		assert.Equal(t, "https://bitbucket.test/pr/1", pr.URL)
		assert.Equal(t, "abcdef1234567890", pr.HeadSHA)
		assert.True(t, pr.Open)
		assert.False(t, pr.Merged)
		require.NotNil(t, pr.CreatedAt)
		assert.Equal(t, now.Unix(), pr.CreatedAt.Unix())
	})
}

func TestGetPullRequest_Errors(t *testing.T) {
	t.Run("handle api error 404", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		repoURL := fmt.Sprintf("%s/projects/proj/repos/repo", server.URL)
		p, _ := NewProvider(repoURL, nil)

		pr, err := p.GetPullRequest(context.Background(), 404)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "404 Not Found")
		assert.Nil(t, pr)
	})

	t.Run("handle malformed json", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{ "id": "not-an-int" }`)) // Type mismatch
		}))
		defer server.Close()

		repoURL := fmt.Sprintf("%s/projects/proj/repos/repo", server.URL)
		p, _ := NewProvider(repoURL, nil)

		_, err := p.GetPullRequest(context.Background(), 1)
		assert.Error(t, err)
	})
}

func TestListPullRequests(t *testing.T) {
	t.Run("successful list with multiple PRs", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			resp := map[string]any{
				"values": []map[string]any{
					{
						"id":    int64(1),
						"state": "OPEN",
						"fromRef": map[string]any{
							"latestCommit": "sha1",
						},
					},
					{
						"id":    int64(2),
						"state": "OPEN",
						"fromRef": map[string]any{
							"latestCommit": "sha2",
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		repoURL := fmt.Sprintf("%s/projects/proj/repos/repo", server.URL)
		p, _ := NewProvider(repoURL, &gitprovider.Options{})

		prs, err := p.ListPullRequests(context.Background(), nil)
		require.NoError(t, err)
		assert.Len(t, prs, 2)
		assert.Equal(t, "sha1", prs[0].HeadSHA)
		assert.Equal(t, "sha2", prs[1].HeadSHA)
	})
}

func TestListPullRequests_Filtering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"values": []map[string]any{
				{
					"id": 1, "state": "OPEN",
					"fromRef": map[string]any{"id": "refs/heads/feature-1", "latestCommit": "sha1"},
					"toRef":   map[string]any{"id": "refs/heads/main"},
				},
				{
					"id": 2, "state": "OPEN",
					"fromRef": map[string]any{"id": "refs/heads/feature-2", "latestCommit": "sha2"},
					"toRef":   map[string]any{"id": "refs/heads/main"},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	repoURL := fmt.Sprintf("%s/projects/proj/repos/repo", server.URL)
	pInterface, _ := NewProvider(repoURL, nil)
	p, ok := pInterface.(*provider)
	require.True(t, ok)

	t.Run("filter by head branch", func(t *testing.T) {
		prs, err := p.ListPullRequests(context.Background(), &gitprovider.ListPullRequestOptions{
			HeadBranch: "feature-2",
		})
		require.NoError(t, err)
		assert.Len(t, prs, 1)
		assert.Equal(t, int64(2), prs[0].Number)
	})

	t.Run("filter by base branch", func(t *testing.T) {
		prs, err := p.ListPullRequests(context.Background(), &gitprovider.ListPullRequestOptions{
			BaseBranch: "main",
		})
		require.NoError(t, err)
		assert.Len(t, prs, 2) // Both target 'main'
	})
}

func TestMergePullRequest(t *testing.T) {
	t.Run("successful two-step merge", func(t *testing.T) {
		mux := http.NewServeMux()

		// 1. Mock GET for version check
		mux.HandleFunc(
			"/rest/api/1.0/projects/proj/repos/repo/pull-requests/1",
			func(w http.ResponseWriter, _ *http.Request) {
				resp := map[string]any{
					"id":      int64(1),
					"version": 99, // Current version
					"state":   "OPEN",
					"fromRef": map[string]any{"latestCommit": "sha-merged"},
				}
				_ = json.NewEncoder(w).Encode(resp)
			},
		)

		// 2. Mock POST merge call
		mux.HandleFunc(
			"/rest/api/1.0/projects/proj/repos/repo/pull-requests/1/merge",
			func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "99", r.URL.Query().Get("version"))
				resp := map[string]any{
					"id":      int64(1),
					"state":   "MERGED",
					"fromRef": map[string]any{"latestCommit": "sha-merged"},
				}
				_ = json.NewEncoder(w).Encode(resp)
			},
		)

		server := httptest.NewServer(mux)
		defer server.Close()

		repoURL := fmt.Sprintf("%s/projects/proj/repos/repo", server.URL)
		p, _ := NewProvider(repoURL, &gitprovider.Options{})

		pr, merged, err := p.MergePullRequest(context.Background(), 1, nil)
		require.NoError(t, err)
		assert.True(t, merged)
		assert.True(t, pr.Merged)
		assert.Equal(t, "sha-merged", pr.HeadSHA)
	})
}

func TestGetCommitURL(t *testing.T) {
	p := &provider{} // We don't need a full NewProvider for this pure string function

	testCases := []struct {
		name     string
		repoURL  string
		sha      string
		expected string
	}{
		{
			name:     "standard project",
			repoURL:  "https://bitbucket.test/projects/PROJ/repos/repo",
			sha:      "abc1234",
			expected: "https://bitbucket.test/projects/proj/repos/repo/commits/abc1234",
		},
		{
			name:     "personal user",
			repoURL:  "https://bitbucket.test/users/oostj20/repos/kargo",
			sha:      "fed6789",
			expected: "https://bitbucket.test/users/oostj20/repos/kargo/commits/fed6789",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			url, err := p.GetCommitURL(tc.repoURL, tc.sha)
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, url)
		})
	}
}

func TestDoRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check headers
		assert.Equal(t, "Bearer secret-token", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		// Check body was sent correctly
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		assert.Equal(t, "bar", body["foo"])

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	p := &provider{
		token:      "secret-token",
		httpClient: http.DefaultClient,
	}

	resp, err := p.doRequest(context.Background(), http.MethodPost, server.URL, map[string]string{"foo": "bar"})
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestToProviderPR(t *testing.T) {
	p := &provider{}

	// Create a mock timestamp (10:00:00 UTC)
	fixedTime := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	millis := fixedTime.UnixNano() / int64(time.Millisecond)

	bb := &bitbucketPR{
		ID:          99,
		Title:       "Mapping Test",
		Description: "Detailed info",
		State:       "OPEN",
		CreatedDate: millis,
	}
	bb.FromRef.LatestCommit = "sha-99"
	bb.Links.Self = []struct {
		Href string `json:"href"`
	}{{Href: "https://bitbucket.test/self"}}

	pr := p.toProviderPR(bb)

	require.NotNil(t, pr)
	assert.Equal(t, int64(99), pr.Number)
	assert.Equal(t, "sha-99", pr.HeadSHA)
	assert.Equal(t, "https://bitbucket.test/self", pr.URL)
	assert.True(t, pr.Open)
	assert.False(t, pr.Merged)

	// Verify Time Conversion
	require.NotNil(t, pr.CreatedAt)
	assert.Equal(t, fixedTime.Unix(), pr.CreatedAt.Unix())

	// Verify raw object is preserved for Merge() versioning
	assert.Equal(t, bb, pr.Object)
}