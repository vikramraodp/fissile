package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/vikramraodp/fissile/app"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile string
	fissile *app.Fissile
	version string
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "fissile",
	Short: "The BOSH disintegrator",
	Long: `
Fissile converts existing BOSH final or dev releases into docker images.

It does this using just the releases, without a BOSH deployment, CPIs, or a BOSH
agent.
`,
	SilenceErrors: true,
	SilenceUsage:  true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if err := validateBasicFlags(); err != nil {
			return err
		}

		return validateReleaseArgs()
	},
}

// Execute adds all child commands to the root command sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute(f *app.Fissile, v string) error {
	fissile = f
	version = v

	return RootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)

	// Here you will define your flags and configuration settings.
	// Cobra supports Persistent Flags, which, if defined here,
	// will be global for your application.

	RootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.fissile.yaml)")

	RootCmd.PersistentFlags().StringP(
		"role-manifest",
		"m",
		"",
		"Path to a yaml file that details which jobs are used for each instance group.",
	)

	// We can't use slices here because of https://github.com/spf13/viper/issues/112
	RootCmd.PersistentFlags().StringP(
		"release",
		"r",
		"",
		"Path to final or dev BOSH release(s).",
	)

	// We can't use slices here because of https://github.com/spf13/viper/issues/112
	RootCmd.PersistentFlags().StringP(
		"release-name",
		"n",
		"",
		"Name of a dev BOSH release; if empty, default configured dev release name will be used; Final release always use the name in release.MF",
	)

	// We can't use slices here because of https://github.com/spf13/viper/issues/112
	RootCmd.PersistentFlags().StringP(
		"release-version",
		"v",
		"",
		"Version of a dev BOSH release; if empty, the latest dev release will be used; Final release always use the version in release.MF",
	)

	RootCmd.PersistentFlags().StringP(
		"cache-dir",
		"c",
		filepath.Join(os.Getenv("HOME"), ".bosh", "cache"),
		"Local BOSH cache directory.",
	)

	RootCmd.PersistentFlags().StringP(
		"final-releases-dir",
		"",
		filepath.Join(os.Getenv("HOME"), ".final-releases"),
		"Local final releases directory.",
	)

	RootCmd.PersistentFlags().StringP(
		"work-dir",
		"w",
		"/var/fissile",
		"Path to the location of the work directory.",
	)

	RootCmd.PersistentFlags().StringP(
		"repository",
		"p",
		"",
		"Repository name prefix used to create image names.",
	)

	RootCmd.PersistentFlags().StringP(
		"docker-registry",
		"",
		"",
		"Docker registry used when referencing image names",
	)

	RootCmd.PersistentFlags().StringP(
		"docker-username",
		"",
		"",
		"Username for authenticated docker registry",
	)

	RootCmd.PersistentFlags().StringP(
		"docker-password",
		"",
		"",
		"Password for authenticated docker registry",
	)

	RootCmd.PersistentFlags().StringP(
		"docker-organization",
		"",
		"",
		"Docker organization used when referencing image names",
	)

	RootCmd.PersistentFlags().IntP(
		"workers",
		"W",
		0,
		"Number of workers to use; zero means determine based on CPU count.",
	)

	RootCmd.PersistentFlags().StringP(
		"light-opinions",
		"l",
		"",
		"Path to a BOSH deployment manifest file that contains properties to be used as defaults.",
	)

	RootCmd.PersistentFlags().StringP(
		"dark-opinions",
		"d",
		"",
		"Path to a BOSH deployment manifest file that contains properties that should not have opinionated defaults.",
	)

	RootCmd.PersistentFlags().StringP(
		"metrics",
		"M",
		"",
		"Path to a CSV file to store timing metrics into.",
	)

	RootCmd.PersistentFlags().StringP(
		"output",
		"o",
		app.OutputFormatHuman,
		"Choose output format, one of human, json, or yaml (currently only for 'show properties')",
	)

	RootCmd.PersistentFlags().BoolP(
		"verbose",
		"V",
		false,
		"Enable verbose output.",
	)

	viper.BindPFlags(RootCmd.PersistentFlags())
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	initViper(viper.GetViper())
}
func initViper(v *viper.Viper) {
	if cfgFile != "" { // enable ability to specify config file via flag
		v.SetConfigFile(cfgFile)
	}

	v.SetEnvPrefix("FISSILE")

	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.SetConfigName(".fissile") // name of config file (without extension)
	v.AddConfigPath("$HOME")    // adding home directory as first search path
	v.AutomaticEnv()            // read in environment variables that match

	// If a config file is found, read it in.
	if err := v.ReadInConfig(); err == nil {
		if v == viper.GetViper() {
			fmt.Println("Using config file:", viper.ConfigFileUsed())
		}
	}
}

func validateBasicFlags() error {
	fissile.Options.RoleManifest = viper.GetString("role-manifest")
	fissile.Options.Releases = splitNonEmpty(viper.GetString("release"), ",")
	fissile.Options.ReleaseNames = splitNonEmpty(viper.GetString("release-name"), ",")
	fissile.Options.ReleaseVersions = splitNonEmpty(viper.GetString("release-version"), ",")
	fissile.Options.CacheDir = viper.GetString("cache-dir")
	fissile.Options.FinalReleasesDir = viper.GetString("final-releases-dir")
	fissile.Options.WorkDir = viper.GetString("work-dir")
	fissile.Options.RepositoryPrefix = viper.GetString("repository")
	fissile.Options.DockerRegistry = strings.TrimSuffix(viper.GetString("docker-registry"), "/")
	fissile.Options.DockerOrganization = viper.GetString("docker-organization")
	fissile.Options.DockerUsername = viper.GetString("docker-username")
	fissile.Options.DockerPassword = viper.GetString("docker-password")
	fissile.Options.Workers = viper.GetInt("workers")
	fissile.Options.LightOpinions = viper.GetString("light-opinions")
	fissile.Options.DarkOpinions = viper.GetString("dark-opinions")
	fissile.Options.OutputFormat = viper.GetString("output")
	fissile.Options.Metrics = viper.GetString("metrics")
	fissile.Options.Verbose = viper.GetBool("verbose")

	// Set defaults for empty flags
	if fissile.Options.RoleManifest == "" {
		fissile.Options.RoleManifest = filepath.Join(fissile.Options.WorkDir, "role-manifest.yml")
	}

	if fissile.Options.LightOpinions == "" {
		fissile.Options.LightOpinions = filepath.Join(fissile.Options.WorkDir, "opinions.yml")
	}

	if fissile.Options.DarkOpinions == "" {
		fissile.Options.DarkOpinions = filepath.Join(fissile.Options.WorkDir, "dark-opinions.yml")
	}

	if fissile.Options.Workers < 1 {
		fissile.Options.Workers = runtime.NumCPU()
	}

	err := absolutePaths(
		&fissile.Options.RoleManifest,
		&fissile.Options.CacheDir,
		&fissile.Options.WorkDir,
		&fissile.Options.LightOpinions,
		&fissile.Options.DarkOpinions,
		&fissile.Options.Metrics,
	)
	if err == nil {
		fissile.Options.Releases, err = absolutePathsForArray(fissile.Options.Releases)
	}
	return err
}

func validateReleaseArgs() error {
	releasePathsCount := len(fissile.Options.Releases)
	releaseNamesCount := len(fissile.Options.ReleaseNames)
	releaseVersionsCount := len(fissile.Options.ReleaseVersions)

	argList := fmt.Sprintf(
		"validateDevReleaseArgs: paths:%s (%d), names:%s (%d), versions:%s (%d)\n",
		fissile.Options.Releases,
		releasePathsCount,
		fissile.Options.ReleaseNames,
		releaseNamesCount,
		fissile.Options.ReleaseVersions,
		releaseVersionsCount,
	)

	if releaseNamesCount != 0 && releaseNamesCount != releasePathsCount {
		return fmt.Errorf("If you specify custom release names, you need to do it for all of them. Args: %s", argList)
	}

	if releaseVersionsCount != 0 && releaseVersionsCount != releasePathsCount {
		return fmt.Errorf("If you specify custom release versions, you need to do it for all of them. Args: %s", argList)
	}

	return nil
}

func absolutePathsForArray(paths []string) ([]string, error) {
	absolutePaths := make([]string, len(paths))
	for idx, path := range paths {
		absPath, err := absolutePath(path)
		if err != nil {
			return nil, err
		}

		absolutePaths[idx] = absPath
	}

	return absolutePaths, nil
}

func absolutePaths(paths ...*string) error {
	for _, path := range paths {
		absPath, err := absolutePath(*path)
		if err != nil {
			return err
		}

		*path = absPath
	}

	return nil
}

func absolutePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("Error getting absolute path for path %s: %v", path, err)
	}

	return path, nil
}

func splitNonEmpty(value string, separator string) []string {
	s := strings.Split(value, separator)

	var r []string
	for _, str := range s {
		if len(str) != 0 {
			r = append(r, str)
		}
	}
	return r
}
