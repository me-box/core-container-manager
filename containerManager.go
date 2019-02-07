package main

import (
	"context"
	b64 "encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	libDatabox "github.com/me-box/lib-go-databox"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"crypto/rand"
)

type ContainerManager struct {
	cli                 *client.Client
	ArbiterClient       *libDatabox.ArbiterClient
	CoreNetworkClient   *CoreNetworkClient
	CmgrStoreClient     *libDatabox.CoreStoreClient
	Request             *http.Client
	DATABOX_DNS_IP      string
	DATABOX_ROOT_CA_ID  string
	ZMQ_PUBLIC_KEY_ID   string
	ZMQ_PRIVATE_KEY_ID  string
	ARCH                string
	cmStoreURL          string
	Store               *CMStore
	Options             *libDatabox.ContainerManagerOptions
	AppStoreName        string
	CoreIUName          string
	CoreStoreName       string
	InstalledComponents map[string]string
}

// New returns a configured ContainerManager
func NewContainerManager(rootCASecretId string, zmqPublicId string, zmqPrivateId string, opt *libDatabox.ContainerManagerOptions) ContainerManager {

	cli, _ := client.NewEnvClient()

	request := libDatabox.NewDataboxHTTPsAPIWithPaths("/certs/containerManager.crt")
	ac, err := libDatabox.NewArbiterClient("/certs/arbiterToken-container-manager", "/run/secrets/ZMQ_PUBLIC_KEY", "tcp://arbiter:4444")
	libDatabox.ChkErr(err)

	cnc := NewCoreNetworkClient("/certs/arbiterToken-databox-network", request)

	cm := ContainerManager{
		cli:                 cli,
		ArbiterClient:       ac,
		CoreNetworkClient:   cnc,
		Request:             request,
		DATABOX_DNS_IP:      os.Getenv("DATABOX_DNS_IP"),
		DATABOX_ROOT_CA_ID:  rootCASecretId,
		ZMQ_PUBLIC_KEY_ID:   zmqPublicId,
		ZMQ_PRIVATE_KEY_ID:  zmqPrivateId,
		Options:             opt,
		AppStoreName:        "app-store",
		CoreIUName:          "core-ui",
		CoreStoreName:       "core-store",
		InstalledComponents: make(map[string]string),
	}

	if opt.Arch != "" {
		cm.ARCH = "-" + opt.Arch
	} else {
		cm.ARCH = ""
	}

	return cm
}

func (cm ContainerManager) Start() {

	//set debug output
	libDatabox.OutputDebug(cm.Options.EnableDebugLogging)

	//register with core-network
	cm.CoreNetworkClient.RegisterPrivileged()

	//wait for the arbiter
	_, err := cm.WaitForService("arbiter", 10)
	if err != nil {
		libDatabox.Err("Filed to register the cm with the arbiter. " + err.Error())
	}

	//start the export service and register with the Arbiter
	go cm.startExportService()

	//register CM with arbiter - this is needed as the CM now has a store!
	err = cm.ArbiterClient.RegesterDataboxComponent("container-manager", cm.ArbiterClient.ArbiterToken, libDatabox.DataboxTypeApp)
	if err != nil {
		libDatabox.Err("Filed to register the cm with the arbiter. " + err.Error())
	}
	//launch the CM store
	cm.cmStoreURL = cm.launchCMStore()

	//setup the cm to log to the store
	cm.CmgrStoreClient = libDatabox.NewCoreStoreClient(cm.ArbiterClient, "/run/secrets/ZMQ_PUBLIC_KEY", cm.cmStoreURL, false)

	//setup the cmStore
	cm.Store = NewCMStore(cm.CmgrStoreClient)

	//clear the saved slas if needed
	if cm.Options.ClearSLAs && err == nil {
		libDatabox.Info("Clearing SLA database to remove saved apps and drivers")
		err = cm.Store.ClearSLADatabase()
		libDatabox.ChkErr(err)
	}

	//Load the password from the store or create a new one
	password := ""
	if cm.Options.OverridePasword != "" {
		libDatabox.Warn("OverridePasword used!")
		password = cm.Options.OverridePasword
	} else {
		libDatabox.Debug("Getting password from DB")
		password, err = cm.Store.LoadPassword()
		libDatabox.ChkErr(err)
		if password == "" {
			libDatabox.Debug("Password not set genorating one")
			password = cm.genoratePassword()
			libDatabox.Debug("Saving new Password")
			err := cm.Store.SavePassword(password)
			libDatabox.ChkErr(err)
		}
	}
	libDatabox.Info("Password=" + password)

	//expose the CM API through the store before starting the UI
	go CmZestAPI(&cm)

	//start default drivers (driver-app-store, driver-export)
	go cm.launchAppStore()
	go cm.launchUI()

	//start the webUI
	go ServeInsecure()
	go ServeSecure(&cm, password)

	libDatabox.Info("Container Manager Ready and waiting")

	//Reload and saved drivers and app from the cm store
	libDatabox.Info("Restarting saved apps and drivers")
	cm.reloadApps()

	//start crash detectore
	go cm.crashDetectore()

}

//Monitor docker events for crashed apps and drivers
//If an app or driver crashes restart it to fix the
//core-network registration
func (cm ContainerManager) crashDetectore() {

	restartDetected := map[string]string{}
	uninstallDetected := map[string]string{}

	eventChan, _ := cm.cli.Events(context.Background(), types.EventsOptions{})
	for {
		select {
		case msg := <-eventChan:

			if msg.Type == "service" && msg.Action == "remove" {
				libDatabox.Debug("Uninstall detected" + msg.Actor.ID)
				uninstallDetected[msg.Actor.ID] = msg.Actor.ID
			}

			if msg.Type == "container" && msg.Action == "kill" && msg.Actor.Attributes["signal"] == "9" {
				libDatabox.Debug("Restart detected")
				restartDetected[msg.Actor.Attributes["name"]] = msg.Actor.Attributes["name"]
			}

			if msg.Type == "container" && msg.Action == "die" {

				name := msg.Actor.Attributes["com.docker.swarm.service.name"]
				ContName := msg.Actor.Attributes["name"]
				serviceID := msg.Actor.Attributes["com.docker.swarm.service.id"]

				if _, ok := restartDetected[ContName]; ok { //is it being restarted?
					delete(restartDetected, ContName)
					libDatabox.Debug("Not restarting " + name + " this time restart detected")
				} else if _, ok := uninstallDetected[serviceID]; ok { //is it being uninstall?
					delete(uninstallDetected, serviceID)
					libDatabox.Debug("Not restarting " + name + " this time uninstall detected")
				} else if _, ok := msg.Actor.Attributes["databox.type"]; !ok {
					//Its not a databox app or driver do nothing
					libDatabox.Debug("Not restarting " + name + " its not a databox app or driver")
				} else { //looks looks a crash restart it
					libDatabox.Warn("Crash detected for " + name)
					libDatabox.Warn("Restarting " + name)
					cm.Restart(name)
				}

			}
		}
	}

}

// LaunchFromSLA will start a databox app or driver with the reliant stores and grant permissions required as described in the SLA
func (cm ContainerManager) LaunchFromSLA(sla libDatabox.SLA, save bool) error {

	//Make the localContainerName
	localContainerName := sla.Name

	//Make the requiredStoreName if needed
	//TODO check store is supported!!
	requiredStoreName := ""
	if sla.ResourceRequirements.Store != "" {
		requiredStoreName = sla.Name + "-" + sla.ResourceRequirements.Store
	}

	libDatabox.Info("Installing " + localContainerName)

	//Create the networks and attach to the core-network.
	netConf := cm.CoreNetworkClient.PreConfig(localContainerName, sla)

	//start the container
	var service swarm.ServiceSpec
	var serviceOptions types.ServiceCreateOptions
	var requiredNetworks []string
	switch sla.DataboxType {
	case libDatabox.DataboxTypeApp:
		service, serviceOptions, requiredNetworks = cm.getAppConfig(sla, localContainerName, netConf)
	case libDatabox.DataboxTypeDriver:
		service, serviceOptions, requiredNetworks = cm.getDriverConfig(sla, localContainerName, netConf)
	default:
		return errors.New("[LaunchFromSLA] Unsupported image type")
	}

	pullImageIfRequired(service.TaskTemplate.ContainerSpec.Image, cm.Options.DefaultRegistry, cm.Options.DefaultRegistryHost)

	//Check image is available and make some noise if its missing !!!
	if !cm.imageExists(service.TaskTemplate.ContainerSpec.Image) {
		return errors.New("Can't install " + localContainerName + " cant find the image " + service.TaskTemplate.ContainerSpec.Image)
	}

	//Add secrests to container
	service.TaskTemplate.ContainerSpec.Secrets = cm.genorateSecrets(localContainerName, sla.DataboxType)

	//If we need a store lets create one and set the needed environment variables
	if requiredStoreName != "" {
		service.TaskTemplate.ContainerSpec.Env = append(
			service.TaskTemplate.ContainerSpec.Env,
			"DATABOX_ZMQ_ENDPOINT=tcp://"+requiredStoreName+":5555",
		)
		service.TaskTemplate.ContainerSpec.Env = append(
			service.TaskTemplate.ContainerSpec.Env,
			"DATABOX_ZMQ_DEALER_ENDPOINT=tcp://"+requiredStoreName+":5556",
		)
	}

	libDatabox.Debug("networksToConnect" + strings.Join(requiredNetworks, ","))
	cm.CoreNetworkClient.ConnectEndpoints(localContainerName, requiredNetworks)

	//do this after the networks are configured
	if requiredStoreName != "" {
		cm.launchStore(sla.ResourceRequirements.Store, requiredStoreName, netConf)
		cm.WaitForService(requiredStoreName, 10)
	}

	cm.addPermissionsFromSLA(sla)

	_, err := cm.cli.ServiceCreate(context.Background(), service, serviceOptions)
	if err != nil {
		libDatabox.Err("[Error launching] " + localContainerName + " " + err.Error())
		return err
	}

	if save {
		//save the sla for persistence over restarts
		err = cm.Store.SaveSLA(sla)
		libDatabox.ChkErr(err)
	}

	//keep track of installed components
	cm.InstalledComponents[localContainerName] = localContainerName

	libDatabox.Info("Successfully installed " + sla.Name)

	return nil
}

//IsInstalled Checks to see a component has been installled
func (cm *ContainerManager) IsInstalled(name string) bool {
	_, ok := cm.InstalledComponents[name]
	return ok
}

func (cm *ContainerManager) imageExists(image string) bool {
	images, _ := cm.cli.ImageList(context.Background(), types.ImageListOptions{})
	for _, i := range images {
		for _, tag := range i.RepoTags {
			if image == tag {
				//we have the image
				return true
			}
		}
	}
	//sorry
	return false
}

// Restart will restart the databox component, app or driver by service name
func (cm ContainerManager) Restart(name string) error {
	filters := filters.NewArgs()
	filters.Add("label", "com.docker.swarm.service.name="+name)

	contList, _ := cm.cli.ContainerList(context.Background(),
		types.ContainerListOptions{
			Filters: filters,
		})
	if len(contList) < 1 {
		return errors.New("Service " + name + " not running")
	}

	//Stash the old container IP
	oldIP := ""
	for netName := range contList[0].NetworkSettings.Networks {
		serviceName := strings.Replace(name, "-"+cm.CoreStoreName, "", 1)
		if strings.Contains(netName, serviceName) {
			oldIP = contList[0].NetworkSettings.Networks[netName].IPAMConfig.IPv4Address
			libDatabox.Debug("Old IP for " + netName + " is " + oldIP)
		}
	}

	//Stop the container then the service will start a new one
	err := cm.cli.ContainerRemove(context.Background(), contList[0].ID, types.ContainerRemoveOptions{Force: true})
	if err != nil {
		return errors.New("Cannot remove " + name + " " + err.Error())
	}

	//wait for the restarted container to start
	newCont, err := cm.WaitForService(name, 20)
	if err != nil {
		libDatabox.Warn("Failed to restart " + name + " " + err.Error())
		return err
	}

	//found restarted container !!!
	//Stash the new container IP
	newIP := ""
	for netName := range newCont.NetworkSettings.Networks {
		serviceName := strings.Replace(name, "-"+cm.CoreStoreName, "", 1)
		if strings.Contains(netName, serviceName) {
			newIP = newCont.NetworkSettings.Networks[netName].IPAMConfig.IPv4Address
			libDatabox.Debug("IP  IP for " + netName + " is " + newIP)
		}
	}

	return cm.CoreNetworkClient.ServiceRestart(name, oldIP, newIP)
}

// Uninstall will remove the databox app or driver by service name
func (cm ContainerManager) Uninstall(name string) error {

	serFilters := filters.NewArgs()
	serFilters.Add("name", name)
	serList, _ := cm.cli.ServiceList(context.Background(),
		types.ServiceListOptions{
			Filters: serFilters,
		})

	if len(serList) < 1 {
		return errors.New("Service " + name + " not running")
	}

	networkConfig, err := cm.CoreNetworkClient.NetworkOfService(serList[0], serList[0].Spec.Name)
	libDatabox.ChkErr(err)

	err = cm.cli.ServiceRemove(context.Background(), serList[0].ID)
	libDatabox.ChkErr(err)

	//remove secrets
	secFilters := filters.NewArgs()
	secFilters.Add("name", name)
	serviceList, err := cm.cli.ServiceList(context.Background(), types.ServiceListOptions{
		Filters: secFilters,
	})
	for _, s := range serviceList {
		for _, sec := range s.Spec.TaskTemplate.ContainerSpec.Secrets {
			libDatabox.Debug("Removing secrete " + sec.SecretName)
			cm.cli.SecretRemove(context.Background(), sec.SecretID)
		}
	}

	cm.CoreNetworkClient.PostUninstall(name, networkConfig)

	cm.Store.DeleteSLA(name)

	delete(cm.InstalledComponents, name)

	return err
}

// WaitForService will wait for a container to start searching for it by service name.
// If the container is found within the timeout it will return a docker/api/types.Container and nil
// otherwise an error will be returned.
func (cm ContainerManager) WaitForService(name string, timeout int) (types.Container, error) {
	libDatabox.Debug("Waiting for " + name)
	filters := filters.NewArgs()
	filters.Add("label", "com.docker.swarm.service.name="+name)

	//wait for the restarted container to start
	var contList []types.Container
	loopCount := 0
	for {

		contList, _ = cm.cli.ContainerList(context.Background(),
			types.ContainerListOptions{
				Filters: filters,
			})

		if len(contList) > 0 {
			//found something!!

			//give it a bit more time
			//TODO think about using /status here
			time.Sleep(time.Second * 3)
			break
		}

		time.Sleep(time.Second)
		loopCount++
		if loopCount > timeout {
			return types.Container{}, errors.New("Service " + name + " has not restarted after " + strconv.Itoa(timeout) + " seconds !!")
		}
		continue

	}

	return contList[0], nil
}

func (cm ContainerManager) genoratePassword() string {

	b := make([]byte, 24)
	rand.Read(b)
	pass := b64.StdEncoding.EncodeToString(b)
	return pass

}

func (cm ContainerManager) reloadApps() {

	slaList, err := cm.Store.GetAllSLAs()
	libDatabox.ChkErr(err)
	libDatabox.Debug("reloadApps slaList len=" + strconv.Itoa(len(slaList)))

	//launch drivers
	var waitGroup sync.WaitGroup
	LaunchFromSLAandWait := func(sla libDatabox.SLA, wg *sync.WaitGroup) {
		err := cm.LaunchFromSLA(sla, false)
		libDatabox.ChkErr(err)
		wg.Done()
	}

	for _, sla := range slaList {
		if sla.DataboxType == libDatabox.DataboxTypeDriver {
			waitGroup.Add(1)
			go LaunchFromSLAandWait(sla, &waitGroup)
		}
	}
	//wait for all drivers to install before we install tha apps
	waitGroup.Wait()

	//launch apps
	for _, sla := range slaList {
		if sla.DataboxType == libDatabox.DataboxTypeApp {
			waitGroup.Add(1)
			go LaunchFromSLAandWait(sla, &waitGroup)
		}
	}
	waitGroup.Wait()

}

// launchAppStore start the app store driver
func (cm ContainerManager) launchUI() {

	name := cm.CoreIUName

	sla := libDatabox.SLA{
		Name:        name,
		DockerImage: cm.Options.CoreUIImage,
		DataboxType: libDatabox.DataboxTypeApp,
		Datasources: []libDatabox.DataSource{
			libDatabox.DataSource{
				Type:          "databox:func:ServiceStatus",
				Required:      true,
				Name:          "ServiceStatus",
				Clientid:      "CM_API_ServiceStatus",
				Granularities: []string{},
				Hypercat: libDatabox.HypercatItem{
					ItemMetadata: []interface{}{
						libDatabox.RelValPairBool{
							Rel: "urn:X-databox:rels:isFunc",
							Val: true,
						},
						libDatabox.RelValPair{
							Rel: "urn:X-databox:rels:hasDatasourceid",
							Val: "ServiceStatus",
						},
					},
					Href: "tcp://container-manager-" + cm.CoreStoreName + ":5555/",
				},
			},
			libDatabox.DataSource{
				Type:          "databox:func:ListAllDatasources",
				Required:      true,
				Name:          "ListAllDatasources",
				Clientid:      "CM_API_ListAllDatasources",
				Granularities: []string{},
				Hypercat: libDatabox.HypercatItem{
					ItemMetadata: []interface{}{
						libDatabox.RelValPairBool{
							Rel: "urn:X-databox:rels:isFunc",
							Val: true,
						},
						libDatabox.RelValPair{
							Rel: "urn:X-databox:rels:hasDatasourceid",
							Val: "ListAllDatasources",
						},
					},
					Href: "tcp://container-manager-" + cm.CoreStoreName + ":5555/",
				},
			},
			libDatabox.DataSource{
				Type:          "databox:container-manager:api",
				Required:      true,
				Name:          "container-manager:api",
				Clientid:      "CM_API",
				Granularities: []string{},
				Hypercat: libDatabox.HypercatItem{
					ItemMetadata: []interface{}{
						libDatabox.RelValPairBool{
							Rel: "urn:X-databox:rels:isActuator",
							Val: true,
						},
						libDatabox.RelValPair{
							Rel: "urn:X-databox:rels:hasDatasourceid",
							Val: "api",
						},
					},
					Href: "tcp://container-manager-" + cm.CoreStoreName + ":5555/kv/api",
				},
			},
			libDatabox.DataSource{
				Type:          "databox:container-manager:data",
				Required:      true,
				Name:          "container-manager:data",
				Clientid:      "CM_DATA",
				Granularities: []string{},
				Hypercat: libDatabox.HypercatItem{
					ItemMetadata: []interface{}{
						libDatabox.RelValPairBool{
							Rel: "urn:X-databox:rels:isActuator",
							Val: false,
						},
						libDatabox.RelValPair{
							Rel: "urn:X-databox:rels:hasDatasourceid",
							Val: "data",
						},
					},
					Href: "tcp://container-manager-" + cm.CoreStoreName + ":5555/kv/data",
				},
			},
			libDatabox.DataSource{
				Type:          "databox:manifests:app",
				Required:      true,
				Name:          "databox Apps",
				Clientid:      "APPS",
				Granularities: []string{},
				Hypercat: libDatabox.HypercatItem{
					ItemMetadata: []interface{}{
						libDatabox.RelValPair{
							Rel: "urn:X-databox:rels:hasDatasourceid",
							Val: "apps",
						},
					},
					Href: "tcp://" + cm.AppStoreName + "-" + cm.CoreStoreName + ":5555/kv/apps",
				},
			},
			libDatabox.DataSource{
				Type:          "databox:manifests:driver",
				Required:      true,
				Name:          "databox Drivers",
				Clientid:      "DRIVERS",
				Granularities: []string{},
				Hypercat: libDatabox.HypercatItem{
					ItemMetadata: []interface{}{
						libDatabox.RelValPair{
							Rel: "urn:X-databox:rels:hasDatasourceid",
							Val: "drivers",
						},
					},
					Href: "tcp://" + cm.AppStoreName + "-" + cm.CoreStoreName + ":5555/kv/drivers",
				},
			},
			libDatabox.DataSource{
				Type:          "databox:manifests:all",
				Required:      true,
				Name:          "databox manifests",
				Clientid:      "ALL",
				Granularities: []string{},
				Hypercat: libDatabox.HypercatItem{
					ItemMetadata: []interface{}{
						libDatabox.RelValPair{
							Rel: "urn:X-databox:rels:hasDatasourceid",
							Val: "all",
						},
					},
					Href: "tcp://" + cm.AppStoreName + "-" + cm.CoreStoreName + ":5555" + "/kv/all",
				},
			},
		},
	}

	err := cm.LaunchFromSLA(sla, false)
	libDatabox.ChkErr(err)

	_, err = cm.WaitForService(name, 10)
	libDatabox.ChkErr(err)

}

// launchAppStore start the app store driver
func (cm ContainerManager) launchAppStore() {
	name := cm.AppStoreName

	sla := libDatabox.SLA{
		Name:        name,
		DockerImage: cm.Options.AppServerImage,
		DataboxType: libDatabox.DataboxTypeDriver,
		ResourceRequirements: libDatabox.ResourceRequirements{
			Store: cm.CoreStoreName,
		},
		ExternalWhitelist: []libDatabox.ExternalWhitelist{
			libDatabox.ExternalWhitelist{
				Urls:        []string{"https://github.com", "https://www.github.com"},
				Description: "Needed to access the manifests stored on github",
			},
		},
	}

	err := cm.LaunchFromSLA(sla, false)
	libDatabox.ChkErr(err)

	_, err = cm.WaitForService(name, 10)
	libDatabox.ChkErr(err)

}

// launchCMStore start a core-store the the container manager to store its configuration
func (cm ContainerManager) launchCMStore() string {
	//startCMStore

	sla := libDatabox.SLA{
		Name:        "container-manager",
		DataboxType: libDatabox.DataboxTypeDriver,
		ResourceRequirements: libDatabox.ResourceRequirements{
			Store: cm.CoreStoreName,
		},
	}

	requiredStoreName := sla.Name + "-" + sla.ResourceRequirements.Store

	cm.launchStore(cm.CoreStoreName, requiredStoreName, NetworkConfig{NetworkName: "databox-system-net", DNS: cm.DATABOX_DNS_IP})
	cm.addPermissionsFromSLA(sla)

	_, err := cm.WaitForService(requiredStoreName, 10)
	libDatabox.ChkErr(err)

	return "tcp://container-manager-" + cm.CoreStoreName + ":5555"
}

func (cm ContainerManager) calculateImageNameFromSLA(sla libDatabox.SLA) string {

	if strings.Contains(sla.DockerImage, "/") && strings.Contains(sla.DockerImage, ":") {
		//looks like a full tagged image
		return sla.DockerImage
	}

	imageName := ""
	registry := ""
	version := ""

	//work out the registry and image name
	if sla.DockerImage != "" {
		imageName = sla.DockerImage
	} else {
		imageName = sla.Name
	}

	//add the arch
	imageName += cm.ARCH

	//work out registry
	if sla.DockerRegistry != "" {
		registry = sla.DockerRegistry
	} else {
		registry = cm.Options.DefaultRegistry
	}

	//work out the image tag
	if sla.DockerImageTag != "" {
		version = sla.DockerImageTag
	} else {
		version = cm.Options.Version
	}

	return registry + "/" + imageName + ":" + version
}

func (cm ContainerManager) getDriverConfig(sla libDatabox.SLA, localContainerName string, netConf NetworkConfig) (swarm.ServiceSpec, types.ServiceCreateOptions, []string) {

	imageName := cm.calculateImageNameFromSLA(sla)

	service := constructDefaultServiceSpec(localContainerName, imageName, libDatabox.DataboxTypeDriver, cm.Options.Version, netConf)
	service.Name = localContainerName

	service.TaskTemplate.ContainerSpec.Env = append(service.TaskTemplate.ContainerSpec.Env, "DATABOX_STORE_URL="+cm.Options.DefaultAppStore)

	return service, types.ServiceCreateOptions{}, []string{"arbiter"}
}

func (cm ContainerManager) getAppConfig(sla libDatabox.SLA, localContainerName string, netConf NetworkConfig) (swarm.ServiceSpec, types.ServiceCreateOptions, []string) {

	imageName := cm.calculateImageNameFromSLA(sla)

	service := constructDefaultServiceSpec(localContainerName, imageName, libDatabox.DataboxTypeApp, cm.Options.Version, netConf)

	service.Name = localContainerName

	//add datasource info to the env vars and create a list networks this app needs to access
	requiredNetworks := map[string]string{"arbiter": "arbiter", "export-service": "export-service"}

	for _, ds := range sla.Datasources {
		hypercatString, _ := json.Marshal(ds.Hypercat)
		service.TaskTemplate.ContainerSpec.Env = append(
			service.TaskTemplate.ContainerSpec.Env,
			"DATASOURCE_"+ds.Clientid+"="+string(hypercatString),
		)
		parsedURL, _ := url.Parse(ds.Hypercat.Href)
		storeName := parsedURL.Hostname()
		requiredNetworks[storeName] = storeName
	}

	//connect to networks
	networksToConnect := []string{}
	if len(requiredNetworks) > 0 {
		for store := range requiredNetworks {
			networksToConnect = append(networksToConnect, store)
		}
	}

	return service, types.ServiceCreateOptions{}, networksToConnect
}

func (cm ContainerManager) launchStore(requiredStore string, requiredStoreName string, netConf NetworkConfig) string {

	//Check to see if the store already exists !!
	storeFilter := filters.NewArgs()
	storeFilter.Add("name", requiredStoreName)
	stores, _ := cm.cli.ServiceList(context.Background(), types.ServiceListOptions{Filters: storeFilter})
	if len(stores) > 0 {
		//we have made this before just return the requiredStoreName
		return requiredStoreName
	}

	image := cm.Options.DefaultStoreImage

	service := constructDefaultServiceSpec(requiredStoreName, image, libDatabox.DataboxTypeStore, cm.Options.Version, netConf)

	service.Name = requiredStoreName
	//Add secrests to store
	service.TaskTemplate.ContainerSpec.Secrets = cm.genorateSecrets(requiredStoreName, libDatabox.DataboxTypeStore)
	storeName := requiredStoreName

	//Add mounts to store
	service.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{
		mount.Mount{
			Source: requiredStoreName,
			Target: "/database",
			Type:   "volume",
		},
	}

	pullImageIfRequired(service.TaskTemplate.ContainerSpec.Image, cm.Options.DefaultRegistry, cm.Options.DefaultRegistryHost)

	_, err := cm.cli.ServiceCreate(context.Background(), service, types.ServiceCreateOptions{})
	if err != nil {
		libDatabox.Err("Launching store " + requiredStoreName + " " + err.Error())
	}

	return storeName
}

func (cm ContainerManager) createSecret(name string, data []byte, filename string) *swarm.SecretReference {

	filters := filters.NewArgs()
	filters.Add("name", name)
	secrestsList, _ := cm.cli.SecretList(context.Background(), types.SecretListOptions{Filters: filters})
	if len(secrestsList) > 0 {
		//remove any secrets and install new one just in case
		for _, s := range secrestsList {
			cm.cli.SecretRemove(context.Background(), s.ID)
		}
	}

	secret := swarm.SecretSpec{
		Annotations: swarm.Annotations{
			Name: name,
		},
		Data: data,
	}

	secretCreateResponse, err := cm.cli.SecretCreate(context.Background(), secret)
	if err != nil {
		libDatabox.Err("addSecrets createSecret " + err.Error())
	}

	return &swarm.SecretReference{
		SecretID:   secretCreateResponse.ID,
		SecretName: name,
		File: &swarm.SecretReferenceFileTarget{
			Name: filename,
			UID:  "0",
			GID:  "0",
			Mode: 0444,
		},
	}
}

// genorateSecrets creates, if required, all the secrets passed to the databox app drivers and stores
func (cm ContainerManager) genorateSecrets(containerName string, databoxType libDatabox.DataboxType) []*swarm.SecretReference {

	secrets := []*swarm.SecretReference{}

	secrets = append(secrets, &swarm.SecretReference{
		SecretID:   cm.DATABOX_ROOT_CA_ID,
		SecretName: "DATABOX_ROOT_CA",
		File: &swarm.SecretReferenceFileTarget{
			Name: "DATABOX_ROOT_CA",
			UID:  "0",
			GID:  "0",
			Mode: 0444,
		},
	})

	secrets = append(secrets, &swarm.SecretReference{
		SecretID:   cm.ZMQ_PUBLIC_KEY_ID,
		SecretName: "ZMQ_PUBLIC_KEY",
		File: &swarm.SecretReferenceFileTarget{
			Name: "ZMQ_PUBLIC_KEY",
			UID:  "0",
			GID:  "0",
			Mode: 0444,
		},
	})

	cert := GenCert(
		"./certs/containerManager.crt", //TODO Fix this
		containerName,
		[]string{"127.0.0.1"},
		[]string{containerName},
	)
	secrets = append(secrets, cm.createSecret(strings.ToUpper(containerName)+".pem", cert, "DATABOX.pem"))

	rawToken := GenerateArbiterToken()
	b64TokenString := b64.StdEncoding.EncodeToString(rawToken)
	secrets = append(secrets, cm.createSecret(strings.ToUpper(containerName)+"_KEY", []byte(b64TokenString), "ARBITER_TOKEN"))

	//update the arbiter with the containers token
	libDatabox.Debug("addSecrets UpdateArbiter " + containerName + " " + b64TokenString + " " + string(databoxType))
	err := cm.ArbiterClient.RegesterDataboxComponent(containerName, b64TokenString, databoxType)
	if err != nil {
		libDatabox.Err("Add Secrets error updating arbiter " + err.Error())
	}

	//Only pass the zmq private key to stores.
	if databoxType == "store" {
		libDatabox.Debug("[addSecrets] ZMQ_PRIVATE_KEY_ID=" + cm.ZMQ_PRIVATE_KEY_ID)
		secrets = append(secrets, &swarm.SecretReference{
			SecretID:   cm.ZMQ_PRIVATE_KEY_ID,
			SecretName: "ZMQ_SECRET_KEY",
			File: &swarm.SecretReferenceFileTarget{
				Name: "ZMQ_SECRET_KEY",
				UID:  "0",
				GID:  "0",
				Mode: 0444,
			},
		})
	}

	return secrets
}

//addPermissionsFromSLA parses a databox SLA and updates the arbiter with the correct permissions
func (cm ContainerManager) addPermissionsFromSLA(sla libDatabox.SLA) {

	var err error

	localContainerName := sla.Name

	//set export permissions from export-whitelist
	if len(sla.ExportWhitelists) > 0 {
		for _, whiteList := range sla.ExportWhitelists {
			caveat := `{"destination":"` + whiteList.Url + `"}`

			libDatabox.Debug("Adding Export permissions for " + localContainerName + " on " + whiteList.Url)

			err = cm.addPermission(localContainerName, "export-service", "/export", "POST", caveat)
			if err != nil {
				libDatabox.Err("Adding write permissions for export-service " + err.Error())
			}

			err = cm.addPermission(localContainerName, "export-service", "/lp/export", "POST", caveat)
			if err != nil {
				libDatabox.Err("Adding write permissions for lp export-service " + err.Error())
			}
		}

	}

	//set export permissions from ExternalWhitelist
	if sla.DataboxType == "driver" && len(sla.ExternalWhitelist) > 0 {
		//TODO move this logic to the coreNetworkClient
		for _, whiteList := range sla.ExternalWhitelist {
			libDatabox.Debug("addPermissionsFromSla adding ExternalWhitelist for " + localContainerName + " on " + strings.Join(whiteList.Urls, ", "))
			externals := []string{}
			for _, u := range whiteList.Urls {
				parsedURL, err := url.Parse(u)
				if err != nil {
					libDatabox.Warn("Error parsing url in ExternalWhitelist")
				}
				externals = append(externals, parsedURL.Hostname())
			}
			err := cm.CoreNetworkClient.ConnectEndpoints(localContainerName, externals)
			libDatabox.ChkErr(err)
		}
	}

	//set read permissions from the sla for DATASOURCES.
	if sla.DataboxType == "app" && len(sla.Datasources) > 0 {

		for _, ds := range sla.Datasources {
			datasourceEndpoint, _ := url.Parse(ds.Hypercat.Href)
			datasourceName := datasourceEndpoint.Path

			libDatabox.Debug("[adding permissions for] datasource" + ds.Name)
			libDatabox.Debug(ds.Name + " IsActuator " + strconv.FormatBool(libDatabox.IsActuator(ds)))

			if libDatabox.IsActuator(ds) { //Deal with Actuators
				libDatabox.Debug("Adding write permissions for Actuator " + datasourceName + "/* on " + datasourceEndpoint.Hostname())
				err = cm.addPermission(localContainerName, datasourceEndpoint.Hostname(), datasourceName+"/*", "POST", "")
				if err != nil {
					libDatabox.Err("Adding write permissions for Actuator " + err.Error())
				}

				libDatabox.Debug("Adding write permissions for Actuator " + datasourceName + " on " + datasourceEndpoint.Hostname())
				err = cm.addPermission(localContainerName, datasourceEndpoint.Hostname(), datasourceName, "POST", "")
				if err != nil {
					libDatabox.Err("Adding write permissions for Actuator " + err.Error())
				}

				libDatabox.Debug("Adding read permissions for " + localContainerName + " on data source " + datasourceName + " on " + datasourceEndpoint.Hostname())
				err = cm.addPermission(localContainerName, datasourceEndpoint.Hostname(), datasourceName, "GET", "")
				if err != nil {
					libDatabox.Err("Adding write permissions for Datasource " + err.Error())
				}

				libDatabox.Debug("Adding read permissions for " + localContainerName + " on data source " + datasourceName + "/* on " + datasourceEndpoint.Hostname())
				err = cm.addPermission(localContainerName, datasourceEndpoint.Hostname(), datasourceName+"/*", "GET", "")
				if err != nil {
					libDatabox.Err("Adding write permissions for Datasource " + err.Error())
				}

			} else if libDatabox.IsFunc(ds) { //Deal with databox functions
				libDatabox.Debug("Adding write permissions for functions request /notification/request/" + ds.Name + "/* on " + datasourceEndpoint.Hostname())
				err = cm.addPermission(localContainerName, datasourceEndpoint.Hostname(), "/notification/request/"+ds.Name+"/*", "POST", "")
				if err != nil {
					libDatabox.Err("Adding write permissions for functions " + err.Error())
				}
				libDatabox.Debug("Adding read permissions for functions response /notification/response/" + ds.Name + "/* on " + datasourceEndpoint.Hostname())
				err = cm.addPermission(localContainerName, datasourceEndpoint.Hostname(), "/notification/response/"+ds.Name+"/*", "GET", "")
				if err != nil {
					libDatabox.Err("Adding write permissions for functions " + err.Error())
				}
			} else {
				libDatabox.Debug("Adding read permissions for " + localContainerName + " on data source " + datasourceName + " on " + datasourceEndpoint.Hostname())
				err = cm.addPermission(localContainerName, datasourceEndpoint.Hostname(), datasourceName, "GET", "")
				if err != nil {
					libDatabox.Err("Adding write permissions for Datasource " + err.Error())
				}

				libDatabox.Debug("Adding read permissions for " + localContainerName + " on data source " + datasourceName + "/* on " + datasourceEndpoint.Hostname())
				err = cm.addPermission(localContainerName, datasourceEndpoint.Hostname(), datasourceName+"/*", "GET", "")
				if err != nil {
					libDatabox.Err("Adding write permissions for Datasource " + err.Error())
				}
			}

		}

	}

	//Add permissions for dependent stores if needed for apps and drivers
	if sla.ResourceRequirements.Store != "" {
		requiredStoreName := sla.Name + "-" + sla.ResourceRequirements.Store

		libDatabox.Debug("Adding read permissions for container-manager on " + requiredStoreName + "/cat")
		err = cm.addPermission("container-manager", requiredStoreName, "/cat", "GET", "")
		if err != nil {
			libDatabox.Err("Adding read permissions for container-manager on /cat " + err.Error())
		}

		libDatabox.Debug("Adding write permissions for dependent store " + localContainerName + " on " + requiredStoreName + "/*")
		err = cm.addPermission(localContainerName, requiredStoreName, "/*", "POST", "")
		if err != nil {
			libDatabox.Err("Adding write permissions for dependent store " + err.Error())
		}

		libDatabox.Debug("Adding delete permissions for dependent store " + localContainerName + " on " + requiredStoreName + "/*")
		err = cm.addPermission(localContainerName, requiredStoreName, "/*", "DELETE", "")
		if err != nil {
			libDatabox.Err("Adding delete permissions for dependent store " + err.Error())
		}

		libDatabox.Debug("Adding read permissions for dependent store " + localContainerName + " on " + requiredStoreName + "/*")
		err = cm.addPermission(localContainerName, requiredStoreName, "/*", "GET", "")
		if err != nil {
			libDatabox.Err("Adding read permissions for dependent store " + err.Error())
		}

	}
}

//addPermission helper function to wraps ArbiterClient.GrantContainerPermissions
func (cm ContainerManager) addPermission(name string, target string, path string, method string, caveat string) error {

	newPermission := libDatabox.ContainerPermissions{
		Name: name,
		Route: libDatabox.Route{
			Target: target,
			Path:   path,
			Method: method,
		},
		Caveat: caveat,
	}

	return cm.ArbiterClient.GrantContainerPermissions(newPermission)

}

func (cm ContainerManager) startExportService() {

	service := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Labels: map[string]string{"databox.type": "system"},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:   cm.Options.ExportServiceImage,
				Env:     []string{"DATABOX_ARBITER_ENDPOINT=tcp://arbiter:4444"},
				Secrets: cm.genorateSecrets("export-service", libDatabox.DataboxTypeStore),
			},
			Networks: []swarm.NetworkAttachmentConfig{swarm.NetworkAttachmentConfig{
				Target: "databox-system-net",
			}},
		},
	}

	service.Name = "export-service"

	serviceOptions := types.ServiceCreateOptions{}

	pullImageIfRequired(service.TaskTemplate.ContainerSpec.Image, cm.Options.DefaultRegistry, cm.Options.DefaultRegistryHost)

	_, err := cm.cli.ServiceCreate(context.Background(), service, serviceOptions)
	libDatabox.ChkErrFatal(err)

}
