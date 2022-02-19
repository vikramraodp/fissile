package model

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"

	"github.com/vikramraodp/fissile/util"
	yaml "gopkg.in/yaml.v2"
)

// ReleaseRef represents a reference to a BOSH release from a manifest
type ReleaseRef struct {
	Name    string `yaml:"name"`
	URL     string `yaml:"url"`
	SHA1    string `yaml:"sha1"`
	Version string `yaml:"version"`
}

// Releases represent a list of releases
type Releases []*Release

// Release represents a BOSH release
type Release struct {
	Jobs               Jobs
	Packages           Packages
	License            ReleaseLicense
	Name               string
	UncommittedChanges bool
	CommitHash         string
	Version            string
	Path               string
	DevBOSHCacheDir    string
	FinalRelease       bool
	manifest           manifest
}

type manifest struct {
	Name               string                        `yaml:"name"`
	Version            string                        `yaml:"version"`
	CommitHash         string                        `yaml:"commit_hash"`
	UncommittedChanges bool                          `yaml:"uncommitted_changes"`
	Jobs               []map[interface{}]interface{} `yaml:"jobs"`
	Packages           []map[interface{}]interface{} `yaml:"packages"`
	License            map[string]string             `yaml:"license"`
}

const (
	jobsDir      = "jobs"
	packagesDir  = "packages"
	manifestFile = "release.MF"
)

// yamlBinaryRegexp is the regexp used to look for the "!binary" YAML tag; see
// loadMetadata() where it is used.
var yamlBinaryRegexp = regexp.MustCompile(`([^!])!binary \|-\n`)

// GetUniqueConfigs returns all unique configs available in a release
func (r *Release) GetUniqueConfigs() map[string]*ReleaseConfig {
	result := map[string]*ReleaseConfig{}

	for _, job := range r.Jobs {
		for _, property := range job.Properties {

			if config, ok := result[property.Name]; ok {
				config.UsageCount++
				config.Jobs = append(config.Jobs, job)
			} else {
				result[property.Name] = &ReleaseConfig{
					Name:        property.Name,
					Jobs:        Jobs{job},
					UsageCount:  1,
					Description: property.Description,
				}
			}
		}
	}

	return result
}

func (r *Release) loadMetadata() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("Error trying to load release %s metadata from YAML manifest %s: %s", r.Name, r.ManifestFilePath(), p)
		}
	}()

	manifestContents, err := ioutil.ReadFile(r.ManifestFilePath())
	if err != nil {
		return err
	}

	// Psych (the Ruby YAML serializer) will incorrectly emit "!binary" when it means "!!binary".
	// This causes the data to be read incorrectly (not base64 decoded), which causes integrity checks to fail.
	// See https://github.com/tenderlove/psych/blob/c1decb1fef5/lib/psych/visitors/yaml_tree.rb#L309
	manifestContents = yamlBinaryRegexp.ReplaceAll(
		manifestContents,
		[]byte("$1!!binary |-\n"),
	)

	err = yaml.Unmarshal([]byte(manifestContents), &r.manifest)
	if err != nil {
		return err
	}

	r.CommitHash = r.manifest.CommitHash
	r.UncommittedChanges = r.manifest.UncommittedChanges
	r.Name = r.manifest.Name
	r.Version = r.manifest.Version

	return nil
}

// LookupPackage will find a package within a BOSH release
func (r *Release) LookupPackage(packageName string) (*Package, error) {
	for _, pkg := range r.Packages {
		if pkg.Name == packageName {
			return pkg, nil
		}
	}

	return nil, fmt.Errorf("Cannot find package %s in release", packageName)
}

// LookupJob will find a job within a BOSH release
func (r *Release) LookupJob(jobName string) (*Job, error) {
	for _, job := range r.Jobs {
		if job.Name == jobName {
			return job, nil
		}
	}

	return nil, fmt.Errorf("Cannot find job %s in release", jobName)
}

func (r *Release) loadJobs() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("Error trying to load release %s jobs from YAML manifest: %s", r.Name, p)
		}
	}()

	for _, job := range r.manifest.Jobs {
		j, err := newJob(r, job)
		if err != nil {
			return err
		}

		r.Jobs = append(r.Jobs, j)
	}

	return nil
}

func (r *Release) loadPackages() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("Error trying to load release %s packages from YAML manifest: %s", r.Name, p)
		}
	}()
	for _, pkg := range r.manifest.Packages {
		p, err := newPackage(r, pkg)
		if err != nil {
			return err
		}

		r.Packages = append(r.Packages, p)
	}

	return nil
}

func (r *Release) loadDependenciesForPackages() error {
	for _, pkg := range r.Packages {
		if err := pkg.loadPackageDependencies(); err != nil {
			return err
		}
	}

	return nil
}

func (r *Release) loadLicense() error {
	r.License.Files = make(map[string][]byte)

	licenseFile, err := os.Open(r.licensePath())
	if os.IsNotExist(err) {
		// There were never licenses to load.
		return nil
	}
	if err != nil {
		return err
	}
	defer licenseFile.Close()

	licenseContents, err := ioutil.ReadFile(r.licensePath())
	if err != nil {
		return err
	}

	licenseFilePath, err := filepath.Rel(r.Path, r.licensePath())
	if err != nil {
		return err
	}

	r.License.Files[licenseFilePath] = licenseContents

	return nil
}

func (r *Release) validatePathStructure() error {
	if err := util.ValidatePath(r.Path, true, "release directory"); err != nil {
		return err
	}

	if err := util.ValidatePath(r.ManifestFilePath(), false, "release manifest file"); err != nil {
		return err
	}

	if err := util.ValidatePath(r.packagesDirPath(), true, "packages directory"); err != nil {
		return err
	}

	return util.ValidatePath(r.jobsDirPath(), true, "jobs directory")
}

func (r *Release) licensePath() string {
	return filepath.Join(r.Path, "LICENSE")
}

func (r *Release) packagesDirPath() string {
	return filepath.Join(r.Path, packagesDir)
}

func (r *Release) jobsDirPath() string {
	return filepath.Join(r.Path, jobsDir)
}

// ManifestFilePath returns the path to the releases manifest
func (r *Release) ManifestFilePath() string {
	if r.FinalRelease {
		return filepath.Join(r.Path, r.getFinalReleaseManifestFilename())
	}

	return filepath.Join(r.getDevReleaseManifestsDir(), r.getDevReleaseManifestFilename())
}

// ReleaseType returns a string identifying the type of the release: Dev or Final.
func (r *Release) ReleaseType() string {
	if r.FinalRelease {
		return "Final"
	}

	return "Dev"
}

// Marshal implements the util.Marshaler interface
func (r *Release) Marshal() (interface{}, error) {
	jobFingerprints := make([]string, 0, len(r.Jobs))
	for _, job := range r.Jobs {
		jobFingerprints = append(jobFingerprints, job.Fingerprint)
	}

	pkgs := make([]string, 0, len(r.Packages))
	for _, pkg := range r.Packages {
		pkgs = append(pkgs, pkg.Fingerprint)
	}

	licenses := make(map[string]string)
	for name, value := range r.License.Files {
		licenses[name] = string(value)
	}

	return map[string]interface{}{
		"jobs":               jobFingerprints,
		"packages":           pkgs,
		"license":            licenses,
		"name":               r.Name,
		"uncommittedChanges": r.UncommittedChanges,
		"commitHash":         r.CommitHash,
		"version":            r.Version,
		"path":               r.Path,
		"devBOSHCacheDir":    r.DevBOSHCacheDir,
	}, nil
}
