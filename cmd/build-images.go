package cmd

import (
	"fmt"
	"strings"

	"github.com/vikramraodp/fissile/app"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// buildImagesCmd represents the images command
var buildImagesCmd = &cobra.Command{
	Use:   "images",
	Short: "Builds Docker images from your BOSH releases.",
	Long: `
This command goes through all the instance group definitions in the role manifest creating a
Dockerfile for each of them and building it.

Each instance group gets a directory ` + "`<work-dir>/dockerfiles`" + `. In each directory one can find
a Dockerfile and a directory structure that gets ADDed to the docker image. The
directory structure contains jobs, packages and all other necessary scripts and
templates.

The images will have a 'instance_group' label useful for filtering.
The entrypoint for each image is ` + "`/opt/fissile/run.sh`" + `.

The images will be tagged: ` + "`<repository>-<instance_group_name>:<SIGNATURE>`" + `.
The SIGNATURE is based on the hashes of all jobs and packages that are included in
the image.

The ` + "`--patch-properties-release`" + ` flag is used to distinguish the patchProperties release/job spec
from other specs.  At most one is allowed.
	`,
	RunE: func(cmd *cobra.Command, args []string) error {
		var opt app.BuildImagesOptions

		opt.NoBuild = buildImagesViper.GetBool("no-build")
		opt.Force = buildImagesViper.GetBool("force")
		opt.PatchPropertiesDirective = buildImagesViper.GetString("patch-properties-release")
		opt.OutputDirectory = buildImagesViper.GetString("output-directory")
		opt.Stemcell = buildImagesViper.GetString("stemcell")
		opt.StemcellID = buildImagesViper.GetString("stemcell-id")
		opt.TagExtra = buildImagesViper.GetString("tag-extra")

		opt.Roles = strings.FieldsFunc(buildImagesViper.GetString("roles"), func(r rune) bool { return r == ',' })

		opt.Labels = make(map[string]string)
		for _, label := range buildImagesViper.GetStringSlice("add-label") {
			parts := strings.Split(label, "=")
			if len(parts) != 2 {
				return fmt.Errorf("invalid label format '%s'. Use: --add-label \"foo=bar\"", label)
			}
			opt.Labels[parts[0]] = parts[1]
		}

		err := fissile.GraphBegin(buildViper.GetString("output-graph"))
		if err != nil {
			return err
		}

		if opt.OutputDirectory != "" && !opt.Force {
			fissile.UI.Printf("--force required when --output-directory is set\n")
			opt.Force = true
		}

		return fissile.BuildImages(opt)
	},
}
var buildImagesViper = viper.New()

func init() {
	initViper(buildImagesViper)

	buildCmd.AddCommand(buildImagesCmd)

	buildImagesCmd.PersistentFlags().BoolP(
		"no-build",
		"N",
		false,
		"If specified, the Dockerfile and assets will be created, but the image won't be built.",
	)

	buildImagesCmd.PersistentFlags().BoolP(
		"force",
		"F",
		false,
		"If specified, image creation will proceed even when images already exist.",
	)

	buildImagesCmd.PersistentFlags().StringP(
		"patch-properties-release",
		"P",
		"",
		"Used to designate a \"patch-properties\" pseudo-job in a particular release.  Format: RELEASE/JOB.",
	)

	// viper is busted w/ string slice, https://github.com/spf13/viper/issues/200
	buildImagesCmd.PersistentFlags().StringP(
		"roles",
		"",
		"",
		"Build only images with the given instance group name; comma separated.",
	)

	buildImagesCmd.PersistentFlags().StringP(
		"output-directory",
		"O",
		"",
		"Output the result as tar files in the given directory rather than building with docker",
	)

	buildImagesCmd.PersistentFlags().StringP(
		"stemcell",
		"s",
		"",
		"The source stemcell",
	)

	buildImagesCmd.PersistentFlags().StringP(
		"stemcell-id",
		"",
		"",
		"Docker image ID for the stemcell (intended for CI)",
	)

	buildImagesCmd.PersistentFlags().StringP(
		"tag-extra",
		"",
		"",
		"Additional information to use in computing the image tags",
	)

	buildImagesCmd.PersistentFlags().StringSliceP(
		"add-label",
		"",
		nil,
		"Additional label which will be set for the base layer image. Format: label=value",
	)

	buildImagesViper.BindPFlags(buildImagesCmd.PersistentFlags())
}
