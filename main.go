package main

import (
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/hashicorp/go-version"
	"github.com/invopop/yaml"
)

var baseDir = "/"

func init() {
	// Allows us to run this locally
	if os.Getenv("SPEAKEASY_ENVIRONMENT") == "local" {
		baseDir = "./"
	}
}

func main() {
	if err := runAction(); err != nil {
		fmt.Printf("::error title=failed::%v\n", err)
		os.Exit(1)
	}
}

func runAction() error {
	debug := os.Getenv("INPUT_DEBUG") == "true"

	if debug {
		for _, env := range os.Environ() {
			fmt.Println(env)
		}
	}

	pinnedSpeakeasyVersion := os.Getenv("INPUT_SPEAKEASY_VERSION")
	openAPIDocLoc := os.Getenv("INPUT_OPENAPI_DOC_LOCATION")
	languages := os.Getenv("INPUT_LANGUAGES")
	createGitRelease := os.Getenv("INPUT_CREATE_RELEASE") == "true"

	accessToken := os.Getenv("INPUT_GITHUB_ACCESS_TOKEN")
	if accessToken == "" {
		return errors.New("github access token is required")
	}

	if err := downloadSpeakeasy(pinnedSpeakeasyVersion); err != nil {
		return err
	}

	langs, err := getAndValidateLanguages(languages)
	if err != nil {
		return err
	}

	g, err := cloneRepo(accessToken)
	if err != nil {
		return err
	}

	genConfigs := loadGeneratorConfigs(langs)

	speakeasyVersion, err := getSpeakeasyVersion()
	if err != nil {
		return err
	}

	docPath, docChecksum, docVersion, err := getOpenAPIFileInfo(openAPIDocLoc)
	if err != nil {
		return err
	}

	langGenerated := map[string]bool{}
	outputs := map[string]string{}

	for lang, cfg := range genConfigs {
		dir := langs[lang]
		langCfg, ok := cfg.Config[lang]
		if !ok {
			langCfg = map[string]string{
				"version": "0.0.0",
			}
			cfg.Config[lang] = langCfg
		}
		sdkVersion := langCfg["version"]

		newVersion, err := checkForChanges(speakeasyVersion, docVersion, docChecksum, sdkVersion, cfg.Config["management"])
		if err != nil {
			return err
		}

		if newVersion != "" {
			fmt.Println("New version detected: ", newVersion)
			outputDir := path.Join(baseDir, "repo", dir)

			cfg.Config[lang]["version"] = newVersion
			if err := writeConfigFile(cfg); err != nil {
				return err
			}

			fmt.Printf("Generating %s SDK in %s\n", lang, outputDir)

			out, err := runSpeakeasyCommand("generate", "sdk", "-s", docPath, "-l", lang, "-o", outputDir)
			if err != nil {
				return fmt.Errorf("error generating sdk: %w - %s", err, out)
			}
			fmt.Println(out)

			outputs[fmt.Sprintf("%s_directory", lang)] = dir

			dirty, err := checkDirDirty(g, dir)
			if err != nil {
				return err
			}

			if dirty {
				langGenerated[lang] = true
			} else {
				cfg.Config[lang]["version"] = sdkVersion
				if err := writeConfigFile(cfg); err != nil {
					return err
				}

				fmt.Printf("Regenerating %s SDK did not result in any changes\n", lang)
			}
		} else {
			fmt.Println("No changes detected")
		}
	}

	regenerated := false

	releaseVersion := ""
	usingGoVersion := false

	if c, ok := genConfigs["go"]; ok {
		releaseVersion = c.Config["go"]["version"]
		usingGoVersion = true
	}

	for lang, cfg := range genConfigs {
		if langGenerated[lang] {
			outputs[lang+"_regenerated"] = "true"

			cfg.Config["management"]["speakeasy-version"] = speakeasyVersion
			cfg.Config["management"]["openapi-version"] = docVersion
			cfg.Config["management"]["openapi-checksum"] = docChecksum

			data, err := yaml.Marshal(cfg.Config)
			if err != nil {
				return fmt.Errorf("error marshaling config: %w", err)
			}

			if err := os.WriteFile(cfg.ConfigPath, data, os.ModePerm); err != nil {
				return fmt.Errorf("error writing config: %w", err)
			}

			if !usingGoVersion {
				if releaseVersion == "" {
					releaseVersion = cfg.Config[lang]["version"]
				} else {
					v, err := version.NewVersion(releaseVersion)
					if err != nil {
						return fmt.Errorf("error parsing version: %w", err)
					}

					v2, err := version.NewVersion(cfg.Config[lang]["version"])
					if err != nil {
						return fmt.Errorf("error parsing version: %w", err)
					}

					if v2.GreaterThan(v) {
						releaseVersion = cfg.Config[lang]["version"]
					}
				}
			}

			regenerated = true
		}
	}

	if regenerated {
		commitHash, err := commitAndPush(g, docVersion, speakeasyVersion, accessToken)
		if err != nil {
			return err
		}

		if createGitRelease {
			if err := createRelease(releaseVersion, commitHash, openAPIDocLoc, docVersion, speakeasyVersion, accessToken); err != nil {
				return err
			}
		}
	}

	if err := setOutputs(outputs); err != nil {
		return err
	}

	return nil
}

func checkForChanges(speakeasyVersion, docVersion, docChecksum, sdkVersion string, mgmtConfig map[string]string) (string, error) {
	if speakeasyVersion != mgmtConfig["speakeasy-version"] || docVersion != mgmtConfig["openapi-version"] || docChecksum != mgmtConfig["openapi-checksum"] {
		bumpMajor := false
		bumpMinor := false
		bumpPatch := false

		if mgmtConfig["speakeasy-version"] == "" {
			bumpMinor = true
		} else {
			previousSpeakeasyV, err := version.NewVersion(mgmtConfig["speakeasy-version"])
			if err != nil {
				return "", fmt.Errorf("error parsing config speakeasy version: %w", err)
			}

			currentSpeakeasyV, err := version.NewVersion(speakeasyVersion)
			if err != nil {
				return "", fmt.Errorf("error parsing speakeasy version: %w", err)
			}

			if currentSpeakeasyV.Segments()[0] > previousSpeakeasyV.Segments()[0] {
				fmt.Printf("Speakeasy version changed detected: %s > %s\n", mgmtConfig["speakeasy-version"], speakeasyVersion)
				bumpMajor = true
			} else if currentSpeakeasyV.Segments()[1] > previousSpeakeasyV.Segments()[1] {
				fmt.Printf("Speakeasy version changed detected: %s > %s\n", mgmtConfig["speakeasy-version"], speakeasyVersion)
				bumpMinor = true
			} else if currentSpeakeasyV.Segments()[2] > previousSpeakeasyV.Segments()[2] {
				fmt.Printf("Speakeasy version changed detected: %s > %s\n", mgmtConfig["speakeasy-version"], speakeasyVersion)
				bumpPatch = true
			}
		}

		docVersionUpdated := false

		if mgmtConfig["openapi-version"] == "" {
			bumpMinor = true
		} else {
			currentDocV, err := version.NewVersion(docVersion)
			// If not a semver then we just deal with the checksum
			if err == nil {
				previousDocV, err := version.NewVersion(mgmtConfig["openapi-version"])
				if err != nil {
					return "", fmt.Errorf("error parsing config openapi version: %w", err)
				}

				if currentDocV.Segments()[0] > previousDocV.Segments()[0] {
					fmt.Printf("OpenAPI doc version changed detected: %s > %s\n", mgmtConfig["openapi-version"], docVersion)
					bumpMajor = true
					docVersionUpdated = true
				} else if currentDocV.Segments()[1] > previousDocV.Segments()[1] {
					fmt.Printf("OpenAPI doc version changed detected: %s > %s\n", mgmtConfig["openapi-version"], docVersion)
					bumpMinor = true
					docVersionUpdated = true
				} else if currentDocV.Segments()[2] > previousDocV.Segments()[2] {
					fmt.Printf("OpenAPI doc version changed detected: %s > %s\n", mgmtConfig["openapi-version"], docVersion)
					bumpPatch = true
					docVersionUpdated = true
				}
			} else {
				fmt.Println("::warning title=invalid_version::openapi version is not a semver")
			}
		}

		if mgmtConfig["openapi-checksum"] == "" {
			bumpMinor = true
		} else if docChecksum != mgmtConfig["openapi-checksum"] {
			bumpPatch = true

			fmt.Printf("OpenAPI doc checksum changed detected: %s > %s\n", mgmtConfig["openapi-checksum"], docChecksum)

			if !docVersionUpdated {
				fmt.Println("::warning title=checksum_changed::openapi checksum changed but version did not")
			}
		}

		var major, minor, patch int

		if sdkVersion != "" {
			sdkV, err := version.NewVersion(sdkVersion)
			if err != nil {
				return "", fmt.Errorf("error parsing sdk version: %w", err)
			}

			major = sdkV.Segments()[0]
			minor = sdkV.Segments()[1]
			patch = sdkV.Segments()[2]
		}

		if bumpMajor {
			fmt.Println("Bumping SDK major version")
			major++
			minor = 0
			patch = 0
		} else if bumpMinor {
			fmt.Println("Bumping SDK minor version")
			minor++
			patch = 0
		} else if bumpPatch {
			fmt.Println("Bumping SDK patch version")
			patch++
		}

		return fmt.Sprintf("%d.%d.%d", major, minor, patch), nil
	}

	return "", nil
}

func setOutputs(outputs map[string]string) error {
	fmt.Println("Setting outputs:")

	outputFile := os.Getenv("GITHUB_OUTPUT")

	f, err := os.OpenFile(outputFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("error opening output file: %w", err)
	}
	defer f.Close()

	for k, v := range outputs {
		out := fmt.Sprintf("%s=%s\n", k, v)
		fmt.Print(out)

		if _, err := f.WriteString(out); err != nil {
			return fmt.Errorf("error writing output: %w", err)
		}
	}

	return nil
}

func writeConfigFile(cfg genConfig) error {
	data, err := yaml.Marshal(cfg.Config)
	if err != nil {
		return fmt.Errorf("error marshaling config: %w", err)
	}

	if err := os.WriteFile(cfg.ConfigPath, data, os.ModePerm); err != nil {
		return fmt.Errorf("error writing config: %w", err)
	}

	return nil
}
