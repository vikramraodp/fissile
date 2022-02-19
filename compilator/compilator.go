package compilator

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vikramraodp/fissile/docker"
	"github.com/vikramraodp/fissile/model"
	"github.com/vikramraodp/fissile/scripts/compilation"
	"github.com/vikramraodp/fissile/util"
	"github.com/SUSE/stampy"
	"github.com/SUSE/termui"
	"github.com/fatih/color"
	workerLib "github.com/jimmysawczuk/worker"
	"github.com/pborman/uuid"
	shutil "github.com/termie/go-shutil"
)

const (
	// ContainerPackagesDir represents the default location of installed BOSH packages
	ContainerPackagesDir = "/var/vcap/packages"
	// ContainerSourceDir is the directory in which the source code will reside when we
	// compile them.  We will add a volume mount there in the container to work around
	// issues with AUFS not emulating a normal filesystem correctly.
	ContainerSourceDir = "/var/vcap/source"
)

// mocked out in tests
var (
	isPackageCompiledHarness = (*Compilator).isPackageCompiled
)

// Compilator represents the BOSH compiler
type Compilator struct {
	dockerManager     *docker.ImageManager
	hostWorkDir       string
	metricsPath       string
	stemcellImageName string
	baseType          string
	fissileVersion    string
	dockerNetworkMode string
	compilePackage    func(*Compilator, *model.Package) error
	packageStorage    *PackageStorage
	streamPackages    bool

	// signalDependencies is a map of
	//    (package fingerprint) -> (channel to close when done)
	// The closing is the signal to dependent packages that
	// this prerequisite is ready for their use.
	//
	// Note, we make sure (see %%) to have only one package per
	// fingerprint.  The fingerprint is based on the package
	// sources. Two formally different packages (in different
	// releases) with the same fingerprint are equivalent and
	// compiling only one is good enough in terms of package
	// dependencies and resulting files.

	signalDependencies map[string]chan struct{}
	keepContainer      bool
	ui                 *termui.UI
	grapher            util.ModelGrapher
}

type compileJob struct {
	workerPackage *workerLib.Package
	pkg           *model.Package
	compilator    *Compilator
	doneCh        chan<- compileResult
	killCh        <-chan struct{}
}

// NewDockerCompilator will create an instance of the Compilator using docker
func NewDockerCompilator(
	dockerManager *docker.ImageManager,
	hostWorkDir string,
	metricsPath string,
	stemcellImageName string,
	baseType string,
	fissileVersion string,
	dockerNetworkMode string,
	keepContainer bool,
	ui *termui.UI,
	grapher util.ModelGrapher,
	packageStorage *PackageStorage,
	streamPackages bool,
) (*Compilator, error) {

	compilator := &Compilator{
		dockerManager:      dockerManager,
		hostWorkDir:        hostWorkDir,
		metricsPath:        metricsPath,
		stemcellImageName:  stemcellImageName,
		baseType:           baseType,
		fissileVersion:     fissileVersion,
		compilePackage:     (*Compilator).compilePackageInDocker,
		streamPackages:     streamPackages,
		dockerNetworkMode:  dockerNetworkMode,
		keepContainer:      keepContainer,
		ui:                 ui,
		grapher:            grapher,
		packageStorage:     packageStorage,
		signalDependencies: make(map[string]chan struct{}),
	}

	return compilator, nil
}

// NewMountNSCompilator will create an instance of the Compilator using a mount
// namespace (Linux only)
func NewMountNSCompilator(
	hostWorkDir string,
	metricsPath string,
	stemcellImageName string,
	baseType string,
	fissileVersion string,
	ui *termui.UI,
	grapher util.ModelGrapher,
	packageStorage *PackageStorage,
) (*Compilator, error) {

	compilator := &Compilator{
		hostWorkDir:        hostWorkDir,
		metricsPath:        metricsPath,
		stemcellImageName:  stemcellImageName,
		baseType:           baseType,
		fissileVersion:     fissileVersion,
		compilePackage:     (*Compilator).compilePackageInMountNS,
		ui:                 ui,
		grapher:            grapher,
		packageStorage:     packageStorage,
		signalDependencies: make(map[string]chan struct{}),
	}

	return compilator, nil
}

var errWorkerAbort = errors.New("worker aborted")

type compileResult struct {
	pkg *model.Package
	err error
}

// Compile concurrency works like this:
// 1 routine producing (todoCh<-)                                  <=> Compile() itself
// n workers consuming (<-todoCh)                                  <=> compileJob.Run()'s
// 1 synchronizer consuming EXACTLY 1 <-doneCh for every <-todoCh  <=> Compile() again.
//
// Dependencies:
// - Packages with the least dependencies are queued first.
// - Workers wait for their dependencies by waiting on a map of
//   broadcasting channels that are closed by the synchronizer when
//   something is done compiling successfully
//   ==> c.signalDependencies [<fingerprint>]
//
// In the event of an error:
// - workers will try to bail out of waiting on <-todo or
//   <-c.signalDependencies[<fingerprint>] early if it finds the killCh has been
//   activated. There is a "race" here to see if the synchronizer will
//   drain <-todoCh or if they will select on <-killCh before
//   <-todoCh. In the worst case, extra packages will be compiled by
//   each active worker. See (**), (xx)
//
//   Note that jobs without dependencies ignore the kill signal. See (xx).
//
// - synchronizer will greedily drain the <-todoCh to starve the
//   workers out and won't wait for the <-doneCh for the N packages it
//   drained.
func (c *Compilator) Compile(workerCount int, releases []*model.Release, instanceGroups model.InstanceGroups, verbose bool) error {
	packages, err := c.removeCompiledPackages(c.gatherPackages(releases, instanceGroups), verbose)

	if err != nil {
		return fmt.Errorf("failed to remove compiled packages: %v", err)
	}
	if 0 == len(packages) {
		c.ui.Println("No package needed to be built")
		return nil
	}
	sort.Sort(packages)

	// Setup the queuing system ...
	doneCh := make(chan compileResult)
	killCh := make(chan struct{})

	workerLib.MaxJobs = workerCount

	worker := workerLib.NewWorker()
	buckets := createDepBuckets(packages)

	// ... load it with the jobs to run ...
	for _, pkg := range buckets {
		worker.Add(compileJob{
			pkg:        pkg,
			compilator: c,
			killCh:     killCh,
			doneCh:     doneCh,
		})
	}

	// ... and start everything
	go func() {
		worker.RunUntilDone()
		close(doneCh)
	}()

	// (**) All jobs push their results into the single doneCh.
	// The code below is a synchronizer which pulls the results
	// from the channel as fast as it can and reports them to the
	// user. In case of an error it signals this back to all jobs
	// by closing killCh. This will cause the remaining jobs to
	// abort when the queing system invokes them.  Note however,
	// that the synchronizer is in a race with the dependency
	// checker in func (j compileJob) Run() (see below), some jobs
	// may still run to regular completion.

	killed := false
	for result := range doneCh {
		if result.err == nil {
			close(c.signalDependencies[result.pkg.Fingerprint])
			c.ui.Printf("%s   > success: %s/%s\n",
				color.YellowString("result"),
				color.GreenString(result.pkg.Release.Name),
				color.GreenString(result.pkg.Name))
			continue
		}

		c.ui.Printf(
			"%s   > failure: %s/%s - %s\n",
			color.YellowString("result"),
			color.RedString(result.pkg.Release.Name),
			color.RedString(result.pkg.Name),
			color.RedString(result.err.Error()),
		)

		err = result.err
		if !killed {
			close(killCh)
			killed = true
		}
	}

	return err
}

func (c *Compilator) gatherPackages(releases []*model.Release, instanceGroups model.InstanceGroups) model.Packages {
	var packages []*model.Package

	for _, release := range releases {
		var releasePackages []*model.Package

		// Get the packages of the release ...
		if instanceGroups != nil { // Conditional for building release images (and easier testing)
			releasePackages = c.gatherPackagesFromInstanceGroups(release, instanceGroups)
		} else {
			releasePackages = release.Packages
		}

		// .. and collect for compilation. (%%) Here we ensure
		// via the source fingerprints that only the first of
		// several equivalent packages is taken.
		for _, pkg := range releasePackages {
			if _, known := c.signalDependencies[pkg.Fingerprint]; !known {
				c.signalDependencies[pkg.Fingerprint] = make(chan struct{})
				packages = append(packages, pkg)
			}
		}
	}

	return packages
}

func (j compileJob) Run() {
	c := j.compilator

	// Metrics: Overall time for the specific job
	var waitSeriesName string
	var runSeriesName string
	if c.metricsPath != "" {
		seriesName := fmt.Sprintf("compile-packages::%s/%s", j.pkg.Release.Name, j.pkg.Name)
		waitSeriesName = fmt.Sprintf("compile-packages::wait::%s/%s", j.pkg.Release.Name, j.pkg.Name)
		runSeriesName = fmt.Sprintf("compile-packages::run::%s/%s", j.pkg.Release.Name, j.pkg.Name)

		stampy.Stamp(c.metricsPath, "fissile", seriesName, "start")
		defer stampy.Stamp(c.metricsPath, "fissile", seriesName, "done")

		stampy.Stamp(c.metricsPath, "fissile", waitSeriesName, "start")
	}

	// (xx) Wait for our deps. Note how without deps the killCh is
	// not checked and ignored. It is also in a race with (**)
	// draining doneCh and actually signaling the kill.

	// Time spent waiting
	for _, dep := range j.pkg.Dependencies {
		done := false
		for !done {
			select {
			case <-j.killCh:
				c.ui.Printf("killed:  %s/%s\n",
					color.MagentaString(j.pkg.Release.Name),
					color.MagentaString(j.pkg.Name))
				j.doneCh <- compileResult{pkg: j.pkg, err: errWorkerAbort}

				if c.metricsPath != "" {
					stampy.Stamp(c.metricsPath, "fissile", waitSeriesName, "done")
				}
				return
			case <-time.After(5 * time.Second):
				c.ui.Printf("waiting: %s/%s - %s\n",
					color.MagentaString(j.pkg.Release.Name),
					color.MagentaString(j.pkg.Name),
					color.MagentaString(dep.Name))
			case <-c.signalDependencies[dep.Fingerprint]:
				c.ui.Printf("depdone: %s/%s - %s\n",
					color.MagentaString(j.pkg.Release.Name),
					color.MagentaString(j.pkg.Name),
					color.MagentaString(dep.Name))
				done = true
			}
		}
	}

	if c.metricsPath != "" {
		stampy.Stamp(c.metricsPath, "fissile", waitSeriesName, "done")
	}

	c.ui.Printf("compile: %s/%s\n",
		color.MagentaString(j.pkg.Release.Name),
		color.MagentaString(j.pkg.Name))

	// Time spent in actual compilation
	if c.metricsPath != "" {
		stampy.Stamp(c.metricsPath, "fissile", runSeriesName, "start")
	}

	exists := false
	if c.packageStorage != nil {
		var err error
		c.ui.Printf("cache: %s %s\n", color.MagentaString("searching for"), j.pkg.Name)
		exists, err = c.packageStorage.Exists(j.pkg)
		if err != nil {
			j.doneCh <- compileResult{pkg: j.pkg, err: err}
		}
	}

	// Check to see whether a package already exists in the configured cache
	// and either download that package or compile and upload it
	if exists {
		c.ui.Printf("cache: downloading %s/%s\n", j.pkg.Release.Name, j.pkg.Name)
		currentProgress := 0
		previousProgress := 0
		downloadErr := c.packageStorage.Download(j.pkg, func(progress float64) {
			if progress == -1 {
				c.ui.Printf("cache: finished downloading %s/%s\n", j.pkg.Release.Name, j.pkg.Name)
				return
			}
			currentProgress = int(progress)
			if currentProgress/20 > previousProgress {
				c.ui.Printf("cache: %s/%s %s \n", j.pkg.Release.Name, j.pkg.Name, color.MagentaString("%d%%", currentProgress))
				previousProgress = currentProgress / 20
			}
		})
		if downloadErr != nil {
			c.ui.Println(color.RedString("Error downloading the package"))
		}

		j.doneCh <- compileResult{pkg: j.pkg, err: downloadErr}

	} else {
		c.ui.Printf("compiling\n")
		var workerErr error
		workerErr = c.compilePackage(c, j.pkg)

		if workerErr == nil && c.packageStorage != nil && c.packageStorage.ReadOnly == false {
			c.ui.Printf("uploading\n")
			workerErr = c.packageStorage.Upload(j.pkg)
		}
		if c.metricsPath != "" {
			stampy.Stamp(c.metricsPath, "fissile", runSeriesName, "done")
		}

		c.ui.Printf("done:    %s/%s\n",
			color.MagentaString(j.pkg.Release.Name),
			color.MagentaString(j.pkg.Name))

		j.doneCh <- compileResult{pkg: j.pkg, err: workerErr}
	}
}

func createDepBuckets(packages []*model.Package) []*model.Package {
	var buckets []*model.Package

	// ruby takes forever and has no deps,
	// so optimize by moving ruby packages to the front
	var rubies []*model.Package

	// topological sort, ensuring that each package X is queued
	// only after all of its dependencies.

	// helper data structures:
	// 1. map: package fingerprint -> #(unqueued deps)
	// 2. map: package fingerprint -> list of using packages (inverted dependencies)
	//
	// The counters in the 1st map are initialized with the number
	// of actual dependencies, and then counted down as
	// these dependencies are queued up.
	//
	// When the counter for a package P reaches 0 then P can be
	// queued, and in turn bumps the counters of all packages
	// using it.
	//
	// The counters additionally serve as flags for when a package
	// is queued/processed, saving us a separate boolean
	// flag. This is done by dropping the respective depCount to -1 (**).

	revDeps := make(map[string][]*model.Package)
	depCount := make(map[string]int)

	// Initialize the depCount first. In the next loop we can use
	// the presence of a package P in depCount as the indicator
	// that this package P is not yet compiled. No need to check
	// with 'isPackageCompiledHarness' and incurring a dependency
	// on a Compilator structure.

	for _, pkg := range packages {
		depCount[pkg.Fingerprint] = 0
	}

	// Finalize the depCount and initialize the map of reverse
	// dependencies.

	for _, pkg := range packages {
		// Note! While the packages we loop over are all not yet
		// compiled (due to coming out of
		// 'removeCompiledPackages') we have to check this for
		// the dependencies as well.  Otherwise we list
		// dependencies which are not real.

		for _, dep := range pkg.Dependencies {
			// (dep in packages)
			// <=> (dep in depCount[])
			// <=> (dep not compiled, use dep)

			if _, known := depCount[dep.Fingerprint]; !known {
				// The package is compiled and thus
				// not a true dependency. Skip it.
				continue
			}

			// Record the true dependency
			depCount[pkg.Fingerprint]++
			revDeps[dep.Fingerprint] = append(revDeps[dep.Fingerprint], pkg)
		}
	}

	// Iterate until we have handled all packages.  We expect each
	// iteration to handle at least one package, because the input
	// is a DAG, i.e. has no cycles. Therefore each iteration will
	// have at least one package with no dependencies, and being
	// handled.

	keepRunning := true
	for keepRunning {
		keepRunning = false

		for _, pkg := range packages {

			// The package either still has dependencies waiting (depCount > 0),
			// or is enqueued and processed ((**) depCount == -1 < 0)
			if depCount[pkg.Fingerprint] != 0 {
				continue
			}

			// depCount == 0, time to
			// - queue the package
			// - notify the outer loop to keep running, and
			// - force the following iterations to ignore
			//   the package (See (**)).
			depCount[pkg.Fingerprint]--
			keepRunning = true

			// notify the users of the queued that another
			// of their dependencies is handled
			for _, usr := range revDeps[pkg.Fingerprint] {
				depCount[usr.Fingerprint]--
			}

			// rubies are special, see notes at top of function
			if strings.HasPrefix(pkg.Name, "ruby-2.") {
				rubies = append(rubies, pkg)
				continue
			}

			// queue regular
			buckets = append(buckets, pkg)
		}
	}

	// prepend rubies to get them out of the way first
	buckets = append(rubies, buckets...)

	return buckets
}

func (c *Compilator) compilePackageInDocker(pkg *model.Package) (err error) {
	// Prepare input dir (package plus deps)
	if err := c.createCompilationDirStructure(pkg); err != nil {
		return err
	}

	if err := c.copyDependencies(pkg); err != nil {
		return err
	}

	// Generate a compilation script
	targetScriptName := "compile.sh"
	hostScriptPath := filepath.Join(pkg.GetTargetPackageSourcesDir(c.hostWorkDir), targetScriptName)
	containerScriptPath := filepath.Join(docker.ContainerInPath, targetScriptName)
	if err := compilation.SaveScript(c.baseType, compilation.CompilationScript, hostScriptPath); err != nil {
		return err
	}

	// Extract package
	extractDir := c.getSourcePackageDir(pkg)
	if _, err := pkg.Extract(extractDir); err != nil {
		return err
	}

	// Run compilation in container
	containerName := c.getPackageContainerName(pkg)

	// in-memory buffer of the log
	log := new(bytes.Buffer)
	logWriter := util.NewSyncedWriter(log)

	stdoutWriter := docker.NewFormattingWriter(
		logWriter,
		func(line string) string {
			return color.GreenString("compilation-%s > %s", color.MagentaString("%s", pkg.Name), color.WhiteString("%s", line))
		},
	)
	stderrWriter := docker.NewFormattingWriter(
		logWriter,
		func(line string) string {
			return color.GreenString("compilation-%s > %s", color.MagentaString("%s", pkg.Name), color.RedString("%s", line))
		},
	)
	sourceMountName := fmt.Sprintf("source_mount-%s", uuid.New())
	mounts := map[string]string{
		pkg.GetTargetPackageSourcesDir(c.hostWorkDir): docker.ContainerInPath,
		pkg.GetPackageCompiledTempDir(c.hostWorkDir):  docker.ContainerOutPath,
		// Add the volume mount to work around AUFS issues.  We will clean
		// the volume up (as long as we're not trying to keep the container
		// around for debugging).  We don't give it an actual directory to mount
		// from, so it will be in some docker-maintained storage.
		sourceMountName: ContainerSourceDir,
	}

	// If we stream the package, don't mount any local files.
	// We assume the docker server is not local. So we stream the package to
	// be compiled, and when done, we stream the compiled bits out.
	volumes := map[string]map[string]string{sourceMountName: nil}
	streamIn := map[string]string{}
	streamOut := map[string]string{}
	if c.streamPackages {
		inVolume := fmt.Sprintf("in_volume-%s", uuid.New())
		outVolume := fmt.Sprintf("out_volume-%s", uuid.New())
		volumes[inVolume] = nil
		volumes[outVolume] = nil

		mounts = map[string]string{
			inVolume:  docker.ContainerInPath,
			outVolume: docker.ContainerOutPath,
		}

		streamIn[pkg.GetTargetPackageSourcesDir(c.hostWorkDir)] = docker.ContainerInPath
		streamOut[docker.ContainerOutPath] = pkg.GetPackageCompiledTempDir(c.hostWorkDir)
	}

	exitCode, container, err := c.dockerManager.RunInContainer(docker.RunInContainerOpts{
		ContainerName: containerName,
		ImageName:     c.stemcellImageName,
		EntryPoint:    []string{},
		Cmd:           []string{"/bin/bash", containerScriptPath, pkg.Name, pkg.Version},
		Mounts:        mounts,
		NetworkMode:   c.dockerNetworkMode,
		Volumes:       volumes,
		KeepContainer: c.keepContainer,
		StdoutWriter:  stdoutWriter,
		StderrWriter:  stderrWriter,
		StreamIn:      streamIn,
		StreamOut:     streamOut,
	})

	if container != nil && (!c.keepContainer || err == nil || exitCode == 0) {
		// Attention. While the assignments to 'err' in the
		// deferal below take effect after the 'return'
		// statements coming later they are visible to the
		// caller, i.e.  override the 'return'ed value,
		// because 'err' is a __named__ return parameter.
		defer func() {
			// Remove container - dockerManager.RemoveContainer does a force-rm

			if removeErr := c.dockerManager.RemoveContainer(container.ID); removeErr != nil {
				if err == nil {
					err = removeErr
				} else {
					err = fmt.Errorf("Error compiling package: %s. Error removing package: %s", err.Error(), removeErr.Error())
				}
			}

			if removeErr := c.dockerManager.RemoveVolumes(container); removeErr != nil {
				if err == nil {
					err = removeErr
				} else {
					err = fmt.Errorf("%s: Error removing volumes for package %s: %s", err, pkg.Name, removeErr)
				}
			}
		}()
	}

	if err != nil {
		log.WriteTo(c.ui)
		return fmt.Errorf("Error compiling package %s: %s", pkg.Name, err.Error())
	}

	if exitCode != 0 {
		log.WriteTo(c.ui)
		return fmt.Errorf("Error - compilation for package %s exited with code %d", pkg.Name, exitCode)
	}

	return os.Rename(
		pkg.GetPackageCompiledTempDir(c.hostWorkDir),
		pkg.GetPackageCompiledDir(c.hostWorkDir))
}

func (c *Compilator) isPackageCompiled(pkg *model.Package) (bool, error) {
	// If compiled package exists on hard disk
	compiledPackagePath := pkg.GetPackageCompiledDir(c.hostWorkDir)
	compiledPackagePathExists, err := validatePath(compiledPackagePath, true, "package path")
	if err != nil {
		return false, err
	}

	if !compiledPackagePathExists {
		return false, nil
	}

	compiledDirEmpty, err := isDirEmpty(compiledPackagePath)
	if err != nil {
		return false, err
	}

	return !compiledDirEmpty, nil
}

func isDirEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return true, err
	}

	defer f.Close()

	_, err = f.Readdir(1)
	if err == io.EOF {
		return true, nil
	}

	return false, err
}

func validatePath(path string, shouldBeDir bool, pathDescription string) (bool, error) {
	pathInfo, err := os.Stat(path)

	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}

		return false, err
	}

	if pathInfo.IsDir() && !shouldBeDir {
		return false, fmt.Errorf("Path %s (%s) points to a directory. It should be a a file", path, pathDescription)
	} else if !pathInfo.IsDir() && shouldBeDir {
		return false, fmt.Errorf("Path %s (%s) points to a file. It should be a directory", path, pathDescription)
	}

	return true, nil
}

// createComplilationDirStructure creates a package structure like this:
// .
// └── <pkg-name>
//    └── <pkg-fingerprint>
//	     ├── compiled
//	     ├── compiled-temp
//	     └── sources
//	         └── var
//	             └── vcap
//	                 ├── packages
//	                 │   └── <dependency-package>
//	                 └── source
func (c *Compilator) createCompilationDirStructure(pkg *model.Package) error {
	dependenciesPackageDir := c.getDependenciesPackageDir(pkg)
	sourcePackageDir := c.getSourcePackageDir(pkg)

	if err := os.MkdirAll(dependenciesPackageDir, 0755); err != nil {
		return err
	}

	return os.MkdirAll(sourcePackageDir, 0755)
}

func (c *Compilator) getDependenciesPackageDir(pkg *model.Package) string {
	return filepath.Join(pkg.GetTargetPackageSourcesDir(c.hostWorkDir), "var", "vcap", "packages")
}

func (c *Compilator) getSourcePackageDir(pkg *model.Package) string {
	return filepath.Join(pkg.GetTargetPackageSourcesDir(c.hostWorkDir), "var", "vcap", "source")
}

func (c *Compilator) copyDependencies(pkg *model.Package) error {
	for _, dep := range pkg.Dependencies {
		depCompiledPath := dep.GetPackageCompiledDir(c.hostWorkDir)
		depDestinationPath := filepath.Join(c.getDependenciesPackageDir(pkg), dep.Name)
		if err := os.RemoveAll(depDestinationPath); err != nil {
			return err
		}

		if err := shutil.CopyTree(
			depCompiledPath,
			depDestinationPath,
			&shutil.CopyTreeOptions{
				Symlinks:               true,
				Ignore:                 nil,
				CopyFunction:           shutil.Copy,
				IgnoreDanglingSymlinks: false},
		); err != nil {
			return err
		}
	}

	return nil
}

// baseCompilationContainerName will return the compilation container's name
func (c *Compilator) baseCompilationContainerName() string {
	return util.SanitizeDockerName(fmt.Sprintf("%s-%s", c.stemcellImageName, c.fissileVersion))
}

func (c *Compilator) getPackageContainerName(pkg *model.Package) string {
	// The "-gkp" closer marker ensures that no package name is a
	// prefix of any other package. This ensures that the
	// "strings.HasPrefix" in "func (d *ImageManager)
	// RemoveVolumes" will not misidentify another package's
	// volumes as its own. Example which made trouble without
	// this: "nginx" vs. ngix_webdav".
	return util.SanitizeDockerName(fmt.Sprintf("%s-%s-%s-pkg-%s-gkp", c.baseCompilationContainerName(), pkg.Release.Name, pkg.Release.Version, pkg.Name))
}

// removeCompiledPackages must be called after initPackageMaps as it closes
// the broadcast channels of anything already compiled.
func (c *Compilator) removeCompiledPackages(packages model.Packages, verbose bool) (model.Packages, error) {
	var culledPackages model.Packages
	for _, pkg := range packages {
		compiled, err := isPackageCompiledHarness(c, pkg)
		if err != nil {
			return nil, err
		}

		if c.grapher != nil {
			_ = c.grapher.GraphNode(pkg.Fingerprint, map[string]string{"label": "pkg/" + pkg.Name})
			_ = c.grapher.GraphEdge(c.stemcellImageName, pkg.Fingerprint, nil)
			_ = c.grapher.GraphEdge("release/"+pkg.Release.Name, pkg.Fingerprint, nil)
			for _, dep := range pkg.Dependencies {
				_ = c.grapher.GraphEdge(dep.Fingerprint, pkg.Fingerprint, nil)
			}
		}

		if compiled {
			close(c.signalDependencies[pkg.Fingerprint])
			if verbose {
				c.ui.Printf("found %s in %s\n", color.YellowString(pkg.Name), pkg.GetPackageCompiledDir(c.hostWorkDir))
			}
		} else {
			culledPackages = append(culledPackages, pkg)
			if verbose {
				c.ui.Printf("building %s in %s\n", color.YellowString(pkg.Name), pkg.GetPackageCompiledDir(c.hostWorkDir))
			}
		}
	}

	return culledPackages, nil
}

// gatherPackagesFromInstanceGroups gathers the list of packages of the release, from a list of instance groups, as well as all needed dependencies
// This happens to be a subset of release.Packages, which helps avoid compiling unneeded packages
func (c *Compilator) gatherPackagesFromInstanceGroups(release *model.Release, instanceGroups model.InstanceGroups) []*model.Package {
	var resultPackages []*model.Package
	listedPackages := make(map[string]bool)
	pendingPackages := list.New()

	// Find the initial list of packages to examine (all packages of the release in the manifest)
	for _, instanceGroup := range instanceGroups {
		for _, jobReference := range instanceGroup.JobReferences {
			for _, pkg := range jobReference.Packages {
				if pkg.Release.Name == release.Name {
					pendingPackages.PushBack(pkg)
					if c.grapher != nil {
						_ = c.grapher.GraphEdge(pkg.Fingerprint, jobReference.Fingerprint, nil)
					}
				}
			}
		}
	}

	// For each package, add it to the result list, and check its dependencies
	for elem := pendingPackages.Front(); elem != nil; elem = elem.Next() {
		pkg := elem.Value.(*model.Package)
		if listedPackages[pkg.Name] {
			// Package is already added (via another package's dependencies)
			continue
		}
		resultPackages = append(resultPackages, pkg)
		listedPackages[pkg.Name] = true
		for _, dep := range pkg.Dependencies {
			pendingPackages.PushBack(dep)
		}
	}

	return resultPackages
}
