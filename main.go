package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/cloud66-oss/starter/common"
	"github.com/cloud66-oss/starter/packs"
	"github.com/cloud66-oss/starter/utils"
	"github.com/getsentry/raven-go"
	"github.com/heroku/docker-registry-client/registry"
	"github.com/mitchellh/go-homedir"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

type analysisResult struct {
	Ok                        bool
	Language                  string
	LanguageVersion           string
	SupportedLanguageVersions []string

	Framework        string
	FrameworkVersion string

	Databases      []string
	Warnings       []string
	Dockerfile     string
	Service        string
	StartCommands  []string
	BuildCommands  []string
	DeployCommands []string
}

var (
	flagPath        string
	flagNoPrompt    bool
	flagEnvironment string
	flagTemplates   string
	flagBranch      string
	flagVersion     string
	flagGenerator   string
	flagOverwrite   bool
	flagConfig      string
	flagDaemon      bool
	flagRegistry    bool
	flagChannel     string
	//flags are gone

	config = &Config{}

	// BUILDDATE holds the date starter was built
	BUILDDATE string

	serviceYAMLTemplateDir string
	dockerfileTemplateDir  string
)

const (
	templatePath = "https://raw.githubusercontent.com/cloud66/starter/{{.branch}}/templates/templates.json"
)

func init() {
	flag.StringVar(&flagPath, "p", "", "project path")
	flag.StringVar(&flagConfig, "c", "", "configuration path for the daemon mode")
	flag.BoolVar(&flagNoPrompt, "y", false, "do not prompt user")
	flag.BoolVar(&flagOverwrite, "overwrite", false, "overwrite existing files")
	flag.StringVar(&flagEnvironment, "e", "production", "set project environment")
	flag.StringVar(&flagTemplates, "templates", "", "location of the templates directory")
	flag.StringVar(&flagBranch, "branch", "master", "template branch in github")
	flag.BoolVar(&flagDaemon, "daemon", false, "runs Starter in daemon mode")
	flag.BoolVar(&flagRegistry, "registry", false, "check base images against docker registry")

	flag.StringVar(&flagVersion, "v", "", "version of starter")
	flag.StringVar(&flagChannel, "channel", "", "release channel")
	flag.StringVar(&flagGenerator, "g", "dockerfile", `what kind of files need to be generated by starter:
	-g dockerfile: only the Dockerfile
	-g service: only the service.yml + Dockerfile (cloud 66 specific)
	-g skycap: only the skycap files + Dockerfile (cloud 66 specific)
	-g dockerfile,service,skycap (all files)
	-g kube: starter will generate a kubernetes deployment from service.yml`)

	//sentry DSN setup
	raven.SetDSN("https://b67185420a71409d900c7affe3a4287d:c5402650974e4a179227591ef8c4fd75@sentry.io/187937")
}

// downloading templates from github and putting them into homedir
func GetTemplates(tempDir string) error {
	common.PrintlnL0("Checking templates in %s", tempDir)

	var tv common.TemplateDefinition
	err := common.FetchJSON(strings.Replace(templatePath, "{{.branch}}", flagBranch, -1), nil, &tv)
	if err != nil {
		return err
	}

	// is there a local copy?
	if _, err := os.Stat(filepath.Join(tempDir, "templates.json")); os.IsNotExist(err) {
		// no file. downloading
		common.PrintlnL1("No local templates found. Downloading now.")
		err := os.MkdirAll(tempDir, 0777)
		if err != nil {
			return err
		}

		err = common.DownloadTemplates(tempDir, tv, templatePath, flagBranch)
		if err != nil {
			return err
		}
	}

	// load the local json
	templatesLocal, err := ioutil.ReadFile(filepath.Join(tempDir, "templates.json"))
	if err != nil {
		return err
	}
	var localTv common.TemplateDefinition
	err = json.Unmarshal(templatesLocal, &localTv)
	if err != nil {
		return err
	}

	// compare
	if localTv.Version != tv.Version {
		common.PrintlnL2("Newer templates found. Downloading them now")
		// they are different, we need to download the new ones
		err = common.DownloadTemplates(tempDir, tv, templatePath, flagBranch)
		if err != nil {
			return err
		}
	} else {
		common.PrintlnL1("Local templates are up to date")
	}

	return nil
}

func main() {
	args := os.Args[1:]

	defer recoverPanic()

	if len(args) > 0 && (args[0] == "help" || args[0] == "-h") {
		fmt.Printf("Starter %s (%s) Help\n", utils.Version, utils.Channel)
		flag.PrintDefaults()
		return
	}

	if len(args) > 0 && (args[0] == "version" || args[0] == "-v") {
		fmt.Printf("Starter version: %s (%s)\n", utils.Version, utils.Channel)
		return
	}

	if len(args) > 0 && (args[0] == "update") {
		var channel = utils.Channel
		flag.CommandLine.Parse(os.Args[2:])
		if flagChannel != "" {
			fmt.Println("Channel: ", flagChannel)
			channel = flagChannel
		}

		c := make(chan struct{})
		go func() {
			defer close(c)
			utils.UpdateExec(channel)
		}()
		select {
		case <-c:
			return
		case <-time.After(30 * time.Second):
			fmt.Println("Update timed out")
			return
		}
		return
	}

	flag.Parse()

	if flagDaemon && flagConfig != "" {
		if _, err := os.Stat(flagConfig); os.IsNotExist(err) {
			common.PrintError("Configuration directory not found: %s", flagConfig)
			os.Exit(1)
		}

		common.PrintL0("Using %s for configuration", flagConfig)
		conf, err := ReadFromFile(flagConfig)
		if err != nil {
			common.PrintError("Failed to load configuration file due to %s", err.Error())
			os.Exit(1)
		}
		*config = *conf
	} else {
		config.SetDefaults()
	}

	common.PrintlnTitle("Starter (c) 2019 Cloud66 Inc.")

	// Run in daemon mode
	if flagDaemon {
		signalChan := make(chan os.Signal, 1)
		cleanupDone := make(chan bool)
		signal.Notify(signalChan, os.Interrupt)
		config.template_path = flagTemplates
		config.use_registry = flagRegistry

		api := NewAPI(config)
		err := api.StartAPI()
		if err != nil {
			common.PrintError("Unable to start the API due to %s", err.Error())
			os.Exit(1)
		}

		go func() {
			for range signalChan {
				common.PrintL0("Received an interrupt, stopping services\n")
				cleanupDone <- true
			}
		}()

		<-cleanupDone
		os.Exit(0)
	}

	result, err := analyze(
		true,
		flagPath,
		flagTemplates,
		flagEnvironment,
		flagNoPrompt,
		flagOverwrite,
		flagGenerator,
		"",
		"",
		flagRegistry)

	if err != nil {
		common.PrintError(err.Error())
		os.Exit(1)
	}
	if len(result.Warnings) > 0 {
		common.PrintlnWarning("Warnings:")
		for _, warning := range result.Warnings {
			common.PrintlnWarning(" * " + warning)
		}
	}

	common.PrintlnL0("Now you can add the newly created Dockerfile to your git")
	common.PrintlnL0("To do that you will need to run the following commands:\n\n")
	fmt.Printf("cd %s\n", flagPath)
	fmt.Println("git add Dockerfile")
	fmt.Println("git commit -m 'Adding Dockerfile'")
	if strings.Contains(flagGenerator, "service") {
		common.PrintlnL0("To create a new Docker Stack with Cloud 66 use the following command:\n\n")
		fmt.Printf("cx stacks create --name='CHANGEME' --environment='%s' --service_yaml=service.yml\n\n", flagEnvironment)
	}

	common.PrintlnTitle("Done")
}

func analyze(
	updateTemplates bool,
	path string,
	templates string,
	environment string,
	noPrompt bool,
	overwrite bool,
	generator string,
	git_repo string,
	git_branch string,
	use_registry bool,
) (*analysisResult, error) {

	if path == "" {
		pwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("Unable to detect current directory path due to %s", err.Error())
		}
		path = pwd
	}

	result := &analysisResult{Ok: false}

	// if templateFolder is specified we're going to use that otherwise download
	if templates == "" {
		homeDir, _ := homedir.Dir()

		templates = filepath.Join(homeDir, ".starter")
		if updateTemplates {
			err := GetTemplates(templates)
			if err != nil {
				return nil, fmt.Errorf("Failed to download latest templates due to %s", err.Error())
			}
		}

		dockerfileTemplateDir = templates
		serviceYAMLTemplateDir = templates

	} else {
		common.PrintlnTitle("Using local templates at %s", templates)
		templates, err := filepath.Abs(templates)
		if err != nil {
			return nil, fmt.Errorf("Failed to use %s for templates due to %s", templates, err.Error())
		}
		dockerfileTemplateDir = templates
		serviceYAMLTemplateDir = templates
	}

	common.PrintlnTitle("Detecting framework for the project at %s", path)

	detectedPacks, err := Detect(path)
	var pack packs.Pack

	// Added so that it will be easier to call the pack directly using the API.
	// Also, with this, avoids looking for other packs than "service_yml" one
	// if the generator flags requires the kubernetes format.
	if strings.Contains(generator, "kube") {
		if len(detectedPacks) > 0 {
			for i := 0; i < len(detectedPacks); i++ {
				if detectedPacks[i].Name() == "service.yml" {
					pack = detectedPacks[i]
				}
			}
			if pack == nil {
				return nil, fmt.Errorf("Failed to detect service.yml\n")
			}
		} else {
			return nil, fmt.Errorf("Failed to detect service.yml\n")
		}
	} else {
		pack, err = choosePack(detectedPacks, noPrompt)
		if pack == nil {
			return nil, fmt.Errorf("Failed to detect supported framework\n")
		}
	}
	if err != nil {
		pack = nil
		return nil, fmt.Errorf("Failed to detect framework due to: %s\n", err.Error())
	}

	// check for Dockerfile (before analysis to avoid wasting time)
	dockerfilePath := filepath.Join(path, "Dockerfile")
	if _, err := os.Stat(dockerfilePath); err == nil && pack.Name() != "docker-compose" && pack.Name() != "service.yml" {
		// file exists. should we overwrite?
		if !overwrite {
			return nil, errors.New("Dockerfile already exists. Use overwrite flag to overwrite it")
		}
	}
	if pack.Name() != "service.yml" {
		serviceYAMLPath := filepath.Join(path, "service.yml")
		if _, err := os.Stat(serviceYAMLPath); err == nil {
			// file exists. should we overwrite?
			if !overwrite {
				return nil, errors.New("service.yml already exists. Use overwrite flag to overwrite it")
			}
		}
	}

	//get all the support language versions
	if use_registry && pack.Name() != "docker-compose" && pack.Name() != "service.yml" {
		url := "https://registry-1.docker.io/"
		username := "" // anonymous
		password := "" // anonymous
		hub, err := registry.New(url, username, password)
		if err != nil {
			return nil, errors.New("can't connect to docker registry to check for allowed base images")
		}

		tags, err := hub.Tags("library/" + pack.Name())
		if err != nil {
			return nil, errors.New("can't find the tags for this pack")
		}
		tags = Filter(tags, func(v string) bool {
			ok, _ := regexp.MatchString(`^\d+.\d+.\d+$`, v)
			return ok
		})

		pack.SetSupportedLanguageVersions(tags)
	}

	err = pack.Analyze(path, environment, !noPrompt, git_repo, git_branch)
	if err != nil {
		return nil, fmt.Errorf("Failed to analyze the project due to: %s", err.Error())
	}

	err = pack.WriteDockerfile(dockerfileTemplateDir, path, !noPrompt)
	if err != nil {
		return nil, fmt.Errorf("Failed to write Dockerfile due to: %s", err.Error())
	}

	if strings.Contains(generator, "service") {
		err = pack.WriteServiceYAML(serviceYAMLTemplateDir, path, !noPrompt) //LUCA
		if err != nil {
			return nil, fmt.Errorf("Failed to write service.yml due to: %s", err.Error())
		}
	}

	if strings.Contains(generator, "kube") {
		_, err = os.Stat("kubernetes.yml")
		if err == nil && !overwrite {
			return nil, fmt.Errorf("kubernetes.yml already exists. Use flag to overwrite.")
		}
		err = pack.WriteKubesConfig(path, !noPrompt)

		if err != nil {
			return nil, fmt.Errorf("Failed to write kubes configuration file due to: %s", err.Error())
		}
	}

	if strings.Contains(generator, "skycap") {
		_, err = os.Stat("starter.bundle")
		if err == nil && !overwrite {
			return nil, fmt.Errorf("Starter bundle file already exist. Use flag to overwrite.")
		}
		err = pack.CreateSkycapFiles(path, templates, flagBranch)

		if err != nil {
			return nil, fmt.Errorf("Failed to write Starter bundle file due to: %s", err.Error())
		}
	}

	if len(pack.GetMessages()) > 0 {
		for _, warning := range pack.GetMessages() {
			result.Warnings = append(result.Warnings, warning)
		}
	}

	result.Language = pack.Name()
	result.LanguageVersion = pack.LanguageVersion()
	result.Framework = pack.Framework()
	result.FrameworkVersion = pack.FrameworkVersion()
	result.Databases = pack.GetDatabases()
	result.StartCommands = pack.GetStartCommands()
	result.SupportedLanguageVersions = pack.GetSupportedLanguageVersions()
	result.BuildCommands = []string{}
	result.DeployCommands = []string{}
	result.Ok = true

	return result, nil
}

func recoverPanic() {
	if utils.Channel != "dev" {
		raven.CapturePanicAndWait(func() {
			if rec := recover(); rec != nil {
				panic(rec)
			}
		}, map[string]string{
			"Version":      utils.Version,
			"Platform":     runtime.GOOS,
			"Architecture": runtime.GOARCH,
			"goversion":    runtime.Version()})
	}
}
