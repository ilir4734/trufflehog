package circleci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/go-errors/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/trufflesecurity/trufflehog/v3/pkg/common"
	"github.com/trufflesecurity/trufflehog/v3/pkg/context"
	"github.com/trufflesecurity/trufflehog/v3/pkg/pb/source_metadatapb"
	"github.com/trufflesecurity/trufflehog/v3/pkg/pb/sourcespb"
	"github.com/trufflesecurity/trufflehog/v3/pkg/sources"
)

const baseURL = "https://circleci.com/api/v1.1/"

type Source struct {
	name     string
	token    string
	sourceId int64
	jobId    int64
	verify   bool
	jobPool  *errgroup.Group
	sources.Progress
	client *http.Client
}

// Ensure the Source satisfies the interface at compile time.
var _ sources.Source = (*Source)(nil)

// Type returns the type of source.
// It is used for matching source types in configuration and job input.
func (s *Source) Type() sourcespb.SourceType {
	return sourcespb.SourceType_SOURCE_TYPE_CIRCLECI
}

func (s *Source) SourceID() int64 {
	return s.sourceId
}

func (s *Source) JobID() int64 {
	return s.jobId
}

// Init returns an initialized CircleCI source.
func (s *Source) Init(_ context.Context, name string, jobId, sourceId int64, verify bool, connection *anypb.Any, concurrency int) error {
	s.name = name
	s.sourceId = sourceId
	s.jobId = jobId
	s.verify = verify
	s.jobPool = &errgroup.Group{}
	s.jobPool.SetLimit(concurrency)
	s.client = common.RetryableHttpClientTimeout(3)

	var conn sourcespb.CircleCI
	if err := anypb.UnmarshalTo(connection, &conn, proto.UnmarshalOptions{}); err != nil {
		return errors.WrapPrefix(err, "error unmarshalling connection", 0)
	}

	switch conn.Credential.(type) {
	case *sourcespb.CircleCI_Token:
		s.token = conn.GetToken()
	}

	return nil
}

// scanErrors is used to collect errors encountered while scanning.
// It ensures that errors are collected in a thread-safe manner.
type scanErrors struct {
	count  uint64
	mu     sync.Mutex
	errors []error
}

func newScanErrors(projects int) *scanErrors {
	return &scanErrors{errors: make([]error, 0, projects)}
}

func (s *scanErrors) add(err error) {
	atomic.AddUint64(&s.count, 1)
	s.mu.Lock()
	s.errors = append(s.errors, err)
	s.mu.Unlock()
}

// Chunks emits chunks of bytes over a channel.
func (s *Source) Chunks(ctx context.Context, chunksChan chan *sources.Chunk) error {
	projects, err := s.projects(ctx)
	if err != nil {
		return fmt.Errorf("error getting projects: %w", err)
	}

	var scanned uint64
	scanErrs := newScanErrors(len(projects))

	for _, proj := range projects {
		proj := proj
		s.jobPool.Go(func() error {
			builds, err := s.buildsForProject(ctx, proj)
			if err != nil {
				scanErrs.add(fmt.Errorf("error getting builds for project %s: %w", proj.RepoName, err))
				return nil
			}

			for _, bld := range builds {
				buildSteps, err := s.stepsForBuild(ctx, proj, bld)
				if err != nil {
					scanErrs.add(fmt.Errorf("error getting steps for build %d: %w", bld.BuildNum, err))
					return nil
				}

				for _, step := range buildSteps {
					for _, action := range step.Actions {
						if err = s.chunkAction(ctx, proj, bld, action, step.Name, chunksChan); err != nil {
							scanErrs.add(fmt.Errorf("error chunking action %v: %w", action, err))
							return nil
						}
					}
				}
			}

			atomic.AddUint64(&scanned, 1)
			log.Debugf("scanned %d/%d projects", scanned, len(projects))
			return nil
		})
	}

	_ = s.jobPool.Wait()
	if scanErrs.count > 0 {
		log.Debugf("encountered %d errors while scanning; errors: %v", scanErrs.count, scanErrs)
	}

	return nil
}

type project struct {
	VCS      string `json:"vcs_type"`
	Username string `json:"username"`
	RepoName string `json:"reponame"`
}

func (s *Source) projects(_ context.Context) ([]project, error) {
	reqURL := fmt.Sprintf("%sprojects", baseURL)
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Circle-Token", s.token)
	req.Header.Set("Accept", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode > 399 && res.StatusCode < 500 {
		return nil, fmt.Errorf("invalid credentials, status %d", res.StatusCode)
	}

	var projects []project
	if err := json.NewDecoder(res.Body).Decode(&projects); err != nil {
		return nil, err
	}

	return projects, nil
}

type build struct {
	BuildNum int `json:"build_num"`
}

func (s *Source) buildsForProject(_ context.Context, proj project) ([]build, error) {
	reqURL := fmt.Sprintf("%sproject/%s/%s/%s", baseURL, proj.VCS, proj.Username, proj.RepoName)
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Circle-Token", s.token)
	req.Header.Set("Accept", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	var builds []build
	if err := json.NewDecoder(res.Body).Decode(&builds); err != nil {
		return nil, err
	}

	return builds, nil
}

type action struct {
	Index     int    `json:"index"`
	OutputURL string `json:"output_url"`
}

type buildStep struct {
	Name    string   `json:"name"`
	Actions []action `json:"actions"`
}

func (s *Source) stepsForBuild(_ context.Context, proj project, bld build) ([]buildStep, error) {
	reqURL := fmt.Sprintf("%sproject/%s/%s/%s/%d", baseURL, proj.VCS, proj.Username, proj.RepoName, bld.BuildNum)
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Circle-Token", s.token)
	req.Header.Set("Accept", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	type buildRes struct {
		Steps []buildStep `json:"steps"`
	}

	var bldRes buildRes
	if err := json.NewDecoder(res.Body).Decode(&bldRes); err != nil {
		return nil, err
	}

	return bldRes.Steps, nil
}

func (s *Source) chunkAction(_ context.Context, proj project, bld build, act action, stepName string, chunksChan chan *sources.Chunk) error {
	req, err := http.NewRequest("GET", act.OutputURL, nil)
	if err != nil {
		return err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	logOutput, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}

	linkURL := fmt.Sprintf("https://app.circleci.com/pipelines/%s/%s/%s/%d", proj.VCS, proj.Username, proj.RepoName, bld.BuildNum)

	chunk := &sources.Chunk{
		SourceType: s.Type(),
		SourceName: s.name,
		SourceID:   s.SourceID(),
		Data:       removeCircleSha1Line(logOutput),
		SourceMetadata: &source_metadatapb.MetaData{
			Data: &source_metadatapb.MetaData_Circleci{
				Circleci: &source_metadatapb.CircleCI{
					VcsType:     proj.VCS,
					Username:    proj.Username,
					Repository:  proj.RepoName,
					BuildNumber: int64(bld.BuildNum),
					BuildStep:   stepName,
					Link:        linkURL,
				},
			},
		},
		Verify: s.verify,
	}

	chunksChan <- chunk

	return nil
}

func removeCircleSha1Line(input []byte) []byte {
	// Split the input slice into a slice of lines.
	lines := bytes.Split(input, []byte("\n"))

	// Iterate over the lines and add the ones that don't contain "CIRCLE_SHA1=" to the result slice.
	result := make([][]byte, 0, len(lines))
	for _, line := range lines {
		if !bytes.Contains(line, []byte("CIRCLE_SHA1=")) {
			result = append(result, line)
		}
	}

	// Join the lines in the result slice and return the resulting slice.
	return bytes.Join(result, []byte("\n"))
}
