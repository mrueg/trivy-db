package amazon

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"golang.org/x/xerrors"

	bolt "github.com/etcd-io/bbolt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/aquasecurity/trivy-db/pkg/db"
	"github.com/aquasecurity/trivy-db/pkg/types"
	"github.com/aquasecurity/trivy-db/pkg/utils"
	"github.com/aquasecurity/vuln-list-update/amazon"
)

func TestMain(m *testing.M) {
	utils.Quiet = true
	os.Exit(m.Run())
}

func TestVulnSrc_Update(t *testing.T) {
	testCases := []struct {
		name           string
		cacheDir       string
		batchUpdateErr error
		expectedError  error
		expectedVulns  []types.Advisory
	}{
		{
			name:          "happy path",
			cacheDir:      "testdata",
			expectedError: nil,
		},
		{
			name:          "cache dir doesnt exist",
			cacheDir:      "badpathdoesnotexist",
			expectedError: errors.New("error in amazon walk: error in file walk: lstat badpathdoesnotexist/vuln-list/amazon: no such file or directory"),
		},
		{
			name:           "unable to save amazon defintions",
			cacheDir:       "testdata",
			batchUpdateErr: errors.New("unable to batch update"),
			expectedError:  errors.New("error in amazon save: error in batch update: unable to batch update"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockDBConfig := new(db.MockDBConfig)
			mockDBConfig.On("BatchUpdate", mock.Anything).Return(tc.batchUpdateErr)
			ac := VulnSrc{dbc: mockDBConfig}

			err := ac.Update(tc.cacheDir)
			switch {
			case tc.expectedError != nil:
				assert.EqualError(t, err, tc.expectedError.Error(), tc.name)
			default:
				assert.NoError(t, err, tc.name)
			}
		})
	}
}

func TestVulnSrc_Get(t *testing.T) {
	type getAdvisoriesInput struct {
		version string
		pkgName string
	}
	type getAdvisoriesOutput struct {
		advisories []types.Advisory
		err        error
	}
	type getAdvisories struct {
		input  getAdvisoriesInput
		output getAdvisoriesOutput
	}

	testCases := []struct {
		name          string
		version       string
		pkgName       string
		getAdvisories getAdvisories
		expectedError error
		expectedVulns []types.Advisory
	}{
		{
			name:    "happy path",
			version: "1",
			pkgName: "curl",
			getAdvisories: getAdvisories{
				input: getAdvisoriesInput{
					version: "amazon linux 1",
					pkgName: "curl",
				},
				output: getAdvisoriesOutput{
					advisories: []types.Advisory{
						{VulnerabilityID: "CVE-2019-0001", FixedVersion: "0.1.2"},
					},
					err: nil,
				},
			},
			expectedError: nil,
			expectedVulns: []types.Advisory{{VulnerabilityID: "CVE-2019-0001", FixedVersion: "0.1.2"}},
		},
		{
			name:    "no advisories are returned",
			version: "2",
			pkgName: "bash",
			getAdvisories: getAdvisories{
				input: getAdvisoriesInput{
					version: "amazon linux 2",
					pkgName: "bash",
				},
				output: getAdvisoriesOutput{advisories: []types.Advisory{}, err: nil},
			},
			expectedError: nil,
			expectedVulns: []types.Advisory{},
		},
		{
			name: "amazon GetAdvisories return an error",
			getAdvisories: getAdvisories{
				input: getAdvisoriesInput{
					version: mock.Anything,
					pkgName: mock.Anything,
				},
				output: getAdvisoriesOutput{
					advisories: []types.Advisory{},
					err:        xerrors.New("unable to get advisories"),
				},
			},
			expectedError: errors.New("failed to get Amazon advisories: unable to get advisories"),
			expectedVulns: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockDBConfig := new(db.MockDBConfig)
			mockDBConfig.On("GetAdvisories",
				tc.getAdvisories.input.version, tc.getAdvisories.input.pkgName).Return(
				tc.getAdvisories.output.advisories, tc.getAdvisories.output.err,
			)

			ac := VulnSrc{dbc: mockDBConfig}
			vuls, err := ac.Get(tc.version, tc.pkgName)

			switch {
			case tc.expectedError != nil:
				assert.EqualError(t, err, tc.expectedError.Error(), tc.name)
			default:
				assert.NoError(t, err, tc.name)
			}
			assert.Equal(t, tc.expectedVulns, vuls, tc.name)
		})
	}
}

func TestSeverityFromPriority(t *testing.T) {
	testCases := map[string]types.Severity{
		"low":       types.SeverityLow,
		"medium":    types.SeverityMedium,
		"important": types.SeverityHigh,
		"critical":  types.SeverityCritical,
		"unknown":   types.SeverityUnknown,
	}
	for k, v := range testCases {
		assert.Equal(t, v, severityFromPriority(k))
	}
}

func TestConstructVersion(t *testing.T) {
	type inputCombination struct {
		epoch   string
		version string
		release string
	}

	testCases := []struct {
		name            string
		inc             inputCombination
		expectedVersion string
	}{
		{
			name: "happy path",
			inc: inputCombination{
				epoch:   "2",
				version: "3",
				release: "master",
			},
			expectedVersion: "2:3-master",
		},
		{
			name: "no epoch",
			inc: inputCombination{
				version: "2",
				release: "master",
			},
			expectedVersion: "2-master",
		},
		{
			name: "no release",
			inc: inputCombination{
				epoch:   "",
				version: "2",
			},
			expectedVersion: "2",
		},
		{
			name: "no epoch and release",
			inc: inputCombination{
				version: "2",
			},
			expectedVersion: "2",
		},
		{
			name:            "no epoch release or version",
			inc:             inputCombination{},
			expectedVersion: "",
		},
	}

	for _, tc := range testCases {
		assert.Equal(t, tc.expectedVersion, constructVersion(tc.inc.epoch, tc.inc.version, tc.inc.release), tc.name)
	}
}

func TestVulnSrc_WalkFunc(t *testing.T) {
	testCases := []struct {
		name             string
		ioReader         io.Reader
		inputPath        string
		expectedALASList []alas
		expectedError    error
		expectedLogs     []string
	}{
		{
			name: "happy path",
			ioReader: strings.NewReader(`{
"id":"123",
"severity":"high"
}`),
			inputPath: "1/2/1",
			expectedALASList: []alas{
				{
					Version: "2",
					ALAS: amazon.ALAS{
						ID:       "123",
						Severity: "high",
					},
				},
			},
			expectedError: nil,
		},
		{
			name:             "amazon returns invalid json",
			ioReader:         strings.NewReader(`invalidjson`),
			inputPath:        "1/2/1",
			expectedALASList: []alas(nil),
			expectedError:    errors.New("failed to decode amazon JSON: invalid character 'i' looking for beginning of value"),
		},
		{
			name:          "unsupported amazon version",
			inputPath:     "foo/bar/baz",
			expectedError: nil,
			expectedLogs:  []string{"unsupported amazon version: bar"},
		},
		{
			name:          "empty path",
			inputPath:     "",
			expectedError: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ac := VulnSrc{}

			err := ac.walkFunc(tc.ioReader, tc.inputPath)
			switch {
			case tc.expectedError != nil:
				assert.EqualError(t, err, tc.expectedError.Error(), tc.name)
			default:
				assert.NoError(t, err, tc.name)
			}

			assert.Equal(t, tc.expectedALASList, ac.alasList, tc.name)
		})
	}
}

func TestVulnSrc_CommitFunc(t *testing.T) {
	testCases := []struct {
		name                      string
		alasList                  []alas
		putAdvisoryErr            error
		putVulnerabilityDetailErr error
		expectedError             error
	}{
		{
			name: "happy path",
			alasList: []alas{
				{
					Version: "123",
					ALAS: amazon.ALAS{
						ID:       "123",
						Severity: "high",
						CveIDs:   []string{"CVE-2020-0001"},
						References: []amazon.Reference{
							{
								ID:    "fooref",
								Href:  "http://foo.bar/baz",
								Title: "bartitle",
							},
						},
						Packages: []amazon.Package{
							{
								Name:    "testpkg",
								Epoch:   "123",
								Version: "456",
								Release: "testing",
							},
						},
					},
				},
			},
		},
		{
			name: "failed to save Amazon advisory, PutNestedBucket() return an error",
			alasList: []alas{
				{
					Version: "123",
					ALAS: amazon.ALAS{
						ID:       "123",
						Severity: "high",
						CveIDs:   []string{"CVE-2020-0001"},
						References: []amazon.Reference{
							{
								ID:    "fooref",
								Href:  "http://foo.bar/baz",
								Title: "bartitle",
							},
						},
						Packages: []amazon.Package{
							{
								Name:    "testpkg",
								Epoch:   "123",
								Version: "456",
								Release: "testing",
							},
						},
					},
				},
			},
			putAdvisoryErr: errors.New("putnestedbucket failed to save"),
			expectedError:  errors.New("failed to save amazon advisory: putnestedbucket failed to save"),
		},
		{
			name: "failed to save Amazon advisory, Put() return an error",
			alasList: []alas{
				{
					Version: "123",
					ALAS: amazon.ALAS{
						ID:       "123",
						Severity: "high",
						CveIDs:   []string{"CVE-2020-0001"},
						References: []amazon.Reference{
							{
								ID:    "fooref",
								Href:  "http://foo.bar/baz",
								Title: "bartitle",
							},
						},
						Packages: []amazon.Package{
							{
								Name:    "testpkg",
								Epoch:   "123",
								Version: "456",
								Release: "testing",
							},
						},
					},
				},
			},
			putVulnerabilityDetailErr: errors.New("failed to commit to db"),
			expectedError:             errors.New("failed to save amazon vulnerability detail: failed to commit to db"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockDBConfig := new(db.MockDBConfig)
			mockDBConfig.On("PutAdvisory",
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
				tc.putAdvisoryErr)
			mockDBConfig.On("PutVulnerabilityDetail",
				mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
				tc.putVulnerabilityDetailErr)
			mockDBConfig.On("PutSeverity",
				mock.Anything, mock.Anything, mock.Anything).Return(nil)

			vs := VulnSrc{dbc: mockDBConfig, alasList: tc.alasList}

			err := vs.commitFunc(&bolt.Tx{WriteFlag: 0})
			switch {
			case tc.expectedError != nil:
				assert.EqualError(t, err, tc.expectedError.Error(), tc.name)
			default:
				assert.NoError(t, err, tc.name)
			}
		})
	}
}
