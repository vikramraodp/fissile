package app

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/vikramraodp/fissile/builder"
	"github.com/vikramraodp/fissile/docker"
	"github.com/vikramraodp/fissile/model"
	"github.com/SUSE/stampy"
	"github.com/fatih/color"
)

// BuildImagesOptions contains all option values for the `fissile build images` command.
type BuildImagesOptions struct {
	Force                    bool
	Labels                   map[string]string
	NoBuild                  bool
	OutputDirectory          string
	PatchPropertiesDirective string
	Roles                    []string
	Stemcell                 string
	StemcellID               string
	TagExtra                 string
}

// BuildImages builds all role images using releases.
func (f *Fissile) BuildImages(opt BuildImagesOptions) error {
	err := f.LoadManifest()
	if err != nil {
		return err
	}
	if f.Manifest == nil || len(f.Manifest.LoadedReleases) == 0 {
		return fmt.Errorf("Releases not loaded")
	}
	if errs := f.Validate(); len(errs) != 0 {
		return errs
	}

	if opt.OutputDirectory != "" {
		err := os.MkdirAll(opt.OutputDirectory, 0755)
		if err != nil {
			if os.IsExist(err) {
				return fmt.Errorf("Output directory %s exists and is not a directory", opt.OutputDirectory)
			}
			if err != nil {
				return fmt.Errorf("Error creating directory %s: %v", opt.OutputDirectory, err)
			}
		}
	}

	if f.Options.Metrics != "" {
		stampy.Stamp(f.Options.Metrics, "fissile", "create-images", "start")
		defer stampy.Stamp(f.Options.Metrics, "fissile", "create-images", "done")
	}

	if opt.StemcellID == "" {
		imageManager, err := docker.NewImageManager()
		if err != nil {
			return err
		}

		stemcellImage, err := imageManager.FindImage(opt.Stemcell)
		if err != nil {
			if _, ok := err.(docker.ErrImageNotFound); ok {
				return fmt.Errorf("Stemcell %v", err)
			}
			return err
		}

		opt.StemcellID = stemcellImage.ID
	}

	packagesImageBuilder := &builder.PackagesImageBuilder{
		RepositoryPrefix:     f.Options.RepositoryPrefix,
		StemcellImageName:    opt.Stemcell,
		StemcellImageID:      opt.StemcellID,
		CompiledPackagesPath: f.StemcellCompilationDir(opt.Stemcell),
		FissileVersion:       f.Version,
	}

	instanceGroups, err := f.Manifest.SelectInstanceGroups(opt.Roles)
	if err != nil {
		return err
	}

	if opt.OutputDirectory == "" {
		err = f.buildPackagesImage(opt, instanceGroups, packagesImageBuilder)
	} else {
		err = f.buildPackagesTarball(opt, instanceGroups, packagesImageBuilder)
	}
	if err != nil {
		return err
	}

	imageName, err := packagesImageBuilder.GetImageName(f.Manifest, instanceGroups, f)
	if err != nil {
		return err
	}

	roleImageBuilder := &builder.RoleImageBuilder{
		BaseImageName:      imageName,
		DarkOpinionsPath:   f.Options.DarkOpinions,
		DockerOrganization: f.Options.DockerOrganization,
		DockerRegistry:     f.Options.DockerRegistry,
		FissileVersion:     f.Version,
		Force:              opt.Force,
		Grapher:            f,
		LightOpinionsPath:  f.Options.LightOpinions,
		ManifestPath:       f.Manifest.ManifestFilePath,
		MetricsPath:        f.Options.Metrics,
		NoBuild:            opt.NoBuild,
		OutputDirectory:    opt.OutputDirectory,
		RepositoryPrefix:   f.Options.RepositoryPrefix,
		TagExtra:           opt.TagExtra,
		UI:                 f.UI,
		WorkerCount:        f.Options.Workers,
	}

	return roleImageBuilder.Build(instanceGroups)
}

// buildPackagesImage builds the docker image for the packages layer
// where all packages are included.
func (f *Fissile) buildPackagesImage(
	opt BuildImagesOptions,
	instanceGroups model.InstanceGroups,
	packagesImageBuilder *builder.PackagesImageBuilder,
) error {

	dockerManager, err := docker.NewImageManager()
	if err != nil {
		return fmt.Errorf("Error connecting to docker: %v", err)
	}

	imageName, err := packagesImageBuilder.GetImageName(f.Manifest, instanceGroups, f)
	if err != nil {
		return fmt.Errorf("Error finding instance group's package name: %v", err)
	}
	if !opt.Force {
		hasImage, err := dockerManager.HasImage(imageName)
		if err != nil {
			return fmt.Errorf("Error looking for packages layer %s: %v", imageName, err)
		}
		if hasImage {
			f.UI.Printf("Packages layer %s already exists. Skipping ...\n", color.YellowString(imageName))
			return nil
		}
	}

	hasImage, err := dockerManager.HasImage(opt.Stemcell)
	if err != nil {
		return fmt.Errorf("Error looking up stemcell image %s: %v", imageName, err)
	}
	if !hasImage {
		return fmt.Errorf("Failed to find stemcell image %s. Did you pull it?", opt.Stemcell)
	}

	if opt.NoBuild {
		f.UI.Println("Skipping packages layer docker image build because of --no-build flag.")
		return nil
	}

	f.UI.Printf("Building packages layer docker image %s ...\n", color.YellowString(imageName))
	log := new(bytes.Buffer)
	stdoutWriter := docker.NewFormattingWriter(log, docker.ColoredBuildStringFunc(imageName))

	tarPopulator := packagesImageBuilder.NewDockerPopulator(instanceGroups, opt.Labels, opt.Force)
	err = dockerManager.BuildImageFromCallback(imageName, stdoutWriter, tarPopulator)
	if err != nil {
		log.WriteTo(f.UI)
		return fmt.Errorf("Error building packages layer docker image: %v", err)
	}
	f.UI.Println(color.GreenString("Done."))

	return nil
}

// buildPackagesTarball builds a tarball snapshot of the build context
// for the docker image for the packages layer where all packages are included.
func (f *Fissile) buildPackagesTarball(
	opt BuildImagesOptions,
	instanceGroups model.InstanceGroups,
	packagesImageBuilder *builder.PackagesImageBuilder,
) error {

	imageName, err := packagesImageBuilder.GetImageName(f.Manifest, instanceGroups, f)
	if err != nil {
		return fmt.Errorf("Error finding instance group's package name: %v", err)
	}
	outputPath := filepath.Join(opt.OutputDirectory, fmt.Sprintf("%s.tar", imageName))

	if !opt.Force {
		info, err := os.Stat(outputPath)
		if err == nil && !info.IsDir() {
			f.UI.Printf("Packages layer %s already exists. Skipping ...\n", color.YellowString(outputPath))
			return nil
		}
	}

	if opt.NoBuild {
		f.UI.Println("Skipping packages layer tarball build because of --no-build flag.")
		return nil
	}

	f.UI.Printf("Building packages layer tarball %s ...\n", color.YellowString(outputPath))

	tarFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("Failed to create tar file %s: %v", outputPath, err)
	}
	defer tarFile.Close()
	tarWriter := tar.NewWriter(tarFile)

	// We always force build all packages here to avoid needing to talk to the
	// docker daemon to figure out what we can keep.
	tarPopulator := packagesImageBuilder.NewDockerPopulator(instanceGroups, opt.Labels, true)
	err = tarPopulator(tarWriter)
	if err != nil {
		return fmt.Errorf("Error writing tar file: %v", err)
	}
	err = tarWriter.Close()
	if err != nil {
		return fmt.Errorf("Error closing tar file: %v", err)
	}
	f.UI.Println(color.GreenString("Done."))

	return nil
}
