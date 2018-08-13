// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package marathon

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	config_util "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/prometheus/prometheus/util/strutil"
)

const (
	// metaLabelPrefix is the meta prefix used for all meta labels in this discovery.
	metaLabelPrefix = model.MetaLabelPrefix + "marathon_"
	// appLabelPrefix is the prefix for the application labels.
	appLabelPrefix = metaLabelPrefix + "app_label_"

	// appLabel is used for the name of the app in Marathon.
	appLabel model.LabelName = metaLabelPrefix + "app"
	// imageLabel is the label that is used for the docker image running the service.
	imageLabel model.LabelName = metaLabelPrefix + "image"
	// portIndexLabel is the integer port index when multiple ports are defined;
	// e.g. PORT1 would have a value of '1'
	portIndexLabel model.LabelName = metaLabelPrefix + "port_index"
	// taskLabel contains the mesos task name of the app instance.
	taskLabel model.LabelName = metaLabelPrefix + "task"

	// portMappingLabelPrefix is the prefix for the application portMappings labels.
	portMappingLabelPrefix = metaLabelPrefix + "port_mapping_label_"
	// portDefinitionLabelPrefix is the prefix for the application portDefinitions labels.
	portDefinitionLabelPrefix = metaLabelPrefix + "port_definition_label_"

	// Constants for instrumentation.
	namespace = "prometheus"
)

var (
	refreshFailuresCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "sd_marathon_refresh_failures_total",
			Help:      "The number of Marathon-SD refresh failures.",
		})
	refreshDuration = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Namespace: namespace,
			Name:      "sd_marathon_refresh_duration_seconds",
			Help:      "The duration of a Marathon-SD refresh in seconds.",
		})
	// DefaultSDConfig is the default Marathon SD configuration.
	DefaultSDConfig = SDConfig{
		RefreshInterval: model.Duration(30 * time.Second),
	}
)

// SDConfig is the configuration for services running on Marathon.
type SDConfig struct {
	Servers          []string                     `yaml:"servers,omitempty"`
	RefreshInterval  model.Duration               `yaml:"refresh_interval,omitempty"`
	AuthToken        config_util.Secret           `yaml:"auth_token,omitempty"`
	AuthTokenFile    string                       `yaml:"auth_token_file,omitempty"`
	HTTPClientConfig config_util.HTTPClientConfig `yaml:",inline"`
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (c *SDConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	*c = DefaultSDConfig
	type plain SDConfig
	err := unmarshal((*plain)(c))
	if err != nil {
		return err
	}
	if len(c.Servers) == 0 {
		return fmt.Errorf("marathon_sd: must contain at least one Marathon server")
	}
	if len(c.AuthToken) > 0 && len(c.AuthTokenFile) > 0 {
		return fmt.Errorf("marathon_sd: at most one of auth_token & auth_token_file must be configured")
	}
	if c.HTTPClientConfig.BasicAuth != nil && (len(c.AuthToken) > 0 || len(c.AuthTokenFile) > 0) {
		return fmt.Errorf("marathon_sd: at most one of basic_auth, auth_token & auth_token_file must be configured")
	}
	if (len(c.HTTPClientConfig.BearerToken) > 0 || len(c.HTTPClientConfig.BearerTokenFile) > 0) && (len(c.AuthToken) > 0 || len(c.AuthTokenFile) > 0) {
		return fmt.Errorf("marathon_sd: at most one of bearer_token, bearer_token_file, auth_token & auth_token_file must be configured")
	}
	return c.HTTPClientConfig.Validate()
}

func init() {
	prometheus.MustRegister(refreshFailuresCount)
	prometheus.MustRegister(refreshDuration)
}

const appListPath string = "/v2/apps/?embed=apps.tasks"

// Discovery provides service discovery based on a Marathon instance.
type Discovery struct {
	client          *http.Client
	servers         []string
	refreshInterval time.Duration
	lastRefresh     map[string]*targetgroup.Group
	appsClient      AppListClient
	logger          log.Logger
}

// NewDiscovery returns a new Marathon Discovery.
func NewDiscovery(conf SDConfig, logger log.Logger) (*Discovery, error) {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	rt, err := config_util.NewRoundTripperFromConfig(conf.HTTPClientConfig, "marathon_sd")
	if err != nil {
		return nil, err
	}

	if len(conf.AuthToken) > 0 {
		rt, err = newAuthTokenRoundTripper(conf.AuthToken, rt)
	} else if len(conf.AuthTokenFile) > 0 {
		rt, err = newAuthTokenFileRoundTripper(conf.AuthTokenFile, rt)
	}
	if err != nil {
		return nil, err
	}

	return &Discovery{
		client:          &http.Client{Transport: rt},
		servers:         conf.Servers,
		refreshInterval: time.Duration(conf.RefreshInterval),
		appsClient:      fetchApps,
		logger:          logger,
	}, nil
}

type authTokenRoundTripper struct {
	authToken config_util.Secret
	rt        http.RoundTripper
}

// newAuthTokenRoundTripper adds the provided auth token to a request.
func newAuthTokenRoundTripper(token config_util.Secret, rt http.RoundTripper) (http.RoundTripper, error) {
	return &authTokenRoundTripper{token, rt}, nil
}

func (rt *authTokenRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	// According to https://docs.mesosphere.com/1.11/security/oss/managing-authentication/
	// DC/OS wants with "token=" a different Authorization header than implemented in httputil/client.go
	// so we set this explicitly here.
	request.Header.Set("Authorization", "token="+string(rt.authToken))

	return rt.rt.RoundTrip(request)
}

type authTokenFileRoundTripper struct {
	authTokenFile string
	rt            http.RoundTripper
}

// newAuthTokenFileRoundTripper adds the auth token read from the file to a request.
func newAuthTokenFileRoundTripper(tokenFile string, rt http.RoundTripper) (http.RoundTripper, error) {
	// fail-fast if we can't read the file.
	_, err := ioutil.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("unable to read auth token file %s: %s", tokenFile, err)
	}
	return &authTokenFileRoundTripper{tokenFile, rt}, nil
}

func (rt *authTokenFileRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	b, err := ioutil.ReadFile(rt.authTokenFile)
	if err != nil {
		return nil, fmt.Errorf("unable to read auth token file %s: %s", rt.authTokenFile, err)
	}
	authToken := strings.TrimSpace(string(b))

	// According to https://docs.mesosphere.com/1.11/security/oss/managing-authentication/
	// DC/OS wants with "token=" a different Authorization header than implemented in httputil/client.go
	// so we set this explicitly here.
	request.Header.Set("Authorization", "token="+authToken)
	return rt.rt.RoundTrip(request)
}

// Run implements the Discoverer interface.
func (d *Discovery) Run(ctx context.Context, ch chan<- []*targetgroup.Group) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.refreshInterval):
			err := d.updateServices(ctx, ch)
			if err != nil {
				level.Error(d.logger).Log("msg", "Error while updating services", "err", err)
			}
		}
	}
}

func (d *Discovery) updateServices(ctx context.Context, ch chan<- []*targetgroup.Group) (err error) {
	t0 := time.Now()
	defer func() {
		refreshDuration.Observe(time.Since(t0).Seconds())
		if err != nil {
			refreshFailuresCount.Inc()
		}
	}()

	targetMap, err := d.fetchTargetGroups()
	if err != nil {
		return err
	}

	all := make([]*targetgroup.Group, 0, len(targetMap))
	for _, tg := range targetMap {
		all = append(all, tg)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case ch <- all:
	}

	// Remove services which did disappear.
	for source := range d.lastRefresh {
		_, ok := targetMap[source]
		if !ok {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- []*targetgroup.Group{{Source: source}}:
				level.Debug(d.logger).Log("msg", "Removing group", "source", source)
			}
		}
	}

	d.lastRefresh = targetMap
	return nil
}

func (d *Discovery) fetchTargetGroups() (map[string]*targetgroup.Group, error) {
	url := RandomAppsURL(d.servers)
	apps, err := d.appsClient(d.client, url)
	if err != nil {
		return nil, err
	}

	groups := AppsToTargetGroups(apps)
	return groups, nil
}

// Task describes one instance of a service running on Marathon.
type Task struct {
	ID          string      `json:"id"`
	Host        string      `json:"host"`
	Ports       []uint32    `json:"ports"`
	IPAddresses []IPAddress `json:"ipAddresses"`
}

// IPAddress describes the address and protocol the container's network interface is bound to.
type IPAddress struct {
	Address string `json:"ipAddress"`
	Proto   string `json:"protocol"`
}

// PortMapping describes in which port the process are binding inside the docker container.
type PortMapping struct {
	Labels        map[string]string `json:"labels"`
	ContainerPort uint32            `json:"containerPort"`
	ServicePort   uint32            `json:"servicePort"`
}

// DockerContainer describes a container which uses the docker runtime.
type DockerContainer struct {
	Image        string        `json:"image"`
	PortMappings []PortMapping `json:"portMappings"`
}

// Container describes the runtime an app in running in.
type Container struct {
	Docker       DockerContainer `json:"docker"`
	PortMappings []PortMapping   `json:"portMappings"`
}

// PortDefinition describes which load balancer port you should access to access the service.
type PortDefinition struct {
	Labels map[string]string `json:"labels"`
	Port   uint32            `json:"port"`
}

// Network describes the name and type of network the container is attached to.
type Network struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

// App describes a service running on Marathon.
type App struct {
	ID              string            `json:"id"`
	Tasks           []Task            `json:"tasks"`
	RunningTasks    int               `json:"tasksRunning"`
	Labels          map[string]string `json:"labels"`
	Container       Container         `json:"container"`
	PortDefinitions []PortDefinition  `json:"portDefinitions"`
	Networks        []Network         `json:"networks"`
}

// isContainerNet checks if the app's first network is set to mode 'container'.
func (app App) isContainerNet() bool {
	return len(app.Networks) > 0 && app.Networks[0].Mode == "container"
}

// AppList is a list of Marathon apps.
type AppList struct {
	Apps []App `json:"apps"`
}

// AppListClient defines a function that can be used to get an application list from marathon.
type AppListClient func(client *http.Client, url string) (*AppList, error)

// fetchApps requests a list of applications from a marathon server.
func fetchApps(client *http.Client, url string) (*AppList, error) {
	request, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}

	if (resp.StatusCode < 200) || (resp.StatusCode >= 300) {
		return nil, fmt.Errorf("Non 2xx status '%v' response during marathon service discovery", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	apps, err := parseAppJSON(body)
	if err != nil {
		return nil, fmt.Errorf("%v in %s", err, url)
	}
	return apps, nil
}

func parseAppJSON(body []byte) (*AppList, error) {
	apps := &AppList{}
	err := json.Unmarshal(body, apps)
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// RandomAppsURL randomly selects a server from an array and creates
// an URL pointing to the app list.
func RandomAppsURL(servers []string) string {
	// TODO: If possible update server list from Marathon at some point.
	server := servers[rand.Intn(len(servers))]
	return fmt.Sprintf("%s%s", server, appListPath)
}

// AppsToTargetGroups takes an array of Marathon apps and converts them into target groups.
func AppsToTargetGroups(apps *AppList) map[string]*targetgroup.Group {
	tgroups := map[string]*targetgroup.Group{}
	for _, a := range apps.Apps {
		group := createTargetGroup(&a)
		tgroups[group.Source] = group
	}
	return tgroups
}

func createTargetGroup(app *App) *targetgroup.Group {
	var (
		targets = targetsForApp(app)
		appName = model.LabelValue(app.ID)
		image   = model.LabelValue(app.Container.Docker.Image)
	)
	tg := &targetgroup.Group{
		Targets: targets,
		Labels: model.LabelSet{
			appLabel:   appName,
			imageLabel: image,
		},
		Source: app.ID,
	}

	for ln, lv := range app.Labels {
		ln = appLabelPrefix + strutil.SanitizeLabelName(ln)
		tg.Labels[model.LabelName(ln)] = model.LabelValue(lv)
	}

	return tg
}

func targetsForApp(app *App) []model.LabelSet {
	targets := make([]model.LabelSet, 0, len(app.Tasks))

	var ports []uint32
	var labels []map[string]string
	var prefix string

	if len(app.Container.PortMappings) != 0 {
		// In Marathon 1.5.x the "container.docker.portMappings" object was moved
		// to "container.portMappings".
		ports, labels = extractPortMapping(app.Container.PortMappings, app.isContainerNet())
		prefix = portMappingLabelPrefix

	} else if len(app.Container.Docker.PortMappings) != 0 {
		// Prior to Marathon 1.5 the port mappings could be found at the path
		// "container.docker.portMappings".
		ports, labels = extractPortMapping(app.Container.Docker.PortMappings, app.isContainerNet())
		prefix = portMappingLabelPrefix

	} else if len(app.PortDefinitions) != 0 {
		// PortDefinitions deprecates the "ports" array and can be used to specify
		// a list of ports with metadata in case a mapping is not required.
		ports = make([]uint32, len(app.PortDefinitions))
		labels = make([]map[string]string, len(app.PortDefinitions))

		for i := 0; i < len(app.PortDefinitions); i++ {
			labels[i] = app.PortDefinitions[i].Labels
			ports[i] = app.PortDefinitions[i].Port
		}

		prefix = portDefinitionLabelPrefix
	}

	// Gather info about the app's 'tasks'. Each instance (container) is considered a task
	// and can be reachable at one or more host:port endpoints.
	for _, t := range app.Tasks {

		// There are no labels to gather if only Ports is defined. (eg. with host networking)
		// Ports can only be gathered from the Task (not from the app) and are guaranteed
		// to be the same across all tasks. If we haven't gathered any ports by now,
		// use the task's ports as the port list.
		if len(ports) == 0 && len(t.Ports) != 0 {
			ports = t.Ports
		}

		// Iterate over the ports we gathered using one of the methods above.
		for i := 0; i < len(ports); i++ {

			// Each port represents a possible Prometheus target.
			targetAddress := targetEndpoint(&t, ports[i], app.isContainerNet())
			target := model.LabelSet{
				model.AddressLabel: model.LabelValue(targetAddress),
				taskLabel:          model.LabelValue(t.ID),
				portIndexLabel:     model.LabelValue(strconv.Itoa(i)),
			}

			// Gather all port labels and set them on the current target.
			// Skip if there are no Marathon labels set on this port.
			if len(labels) > 0 {
				for ln, lv := range labels[i] {
					ln = prefix + strutil.SanitizeLabelName(ln)
					target[model.LabelName(ln)] = model.LabelValue(lv)
				}
			}

			targets = append(targets, target)
		}
	}
	return targets
}

// Generate a target endpoint string in host:port format.
func targetEndpoint(task *Task, port uint32, containerNet bool) string {

	var host string

	// Use the task's ipAddress field when it's in a container network
	if containerNet && len(task.IPAddresses) > 0 {
		host = task.IPAddresses[0].Address
	} else {
		host = task.Host
	}

	return net.JoinHostPort(host, fmt.Sprintf("%d", port))
}

// Get a list of ports and a list of labels from a PortMapping.
func extractPortMapping(portMappings []PortMapping, containerNet bool) ([]uint32, []map[string]string) {

	ports := make([]uint32, len(portMappings))
	labels := make([]map[string]string, len(portMappings))

	for i := 0; i < len(portMappings); i++ {

		labels[i] = portMappings[i].Labels

		if containerNet {
			// If the app is in a container network, connect directly to the container port.
			ports[i] = portMappings[i].ContainerPort
		} else {
			// Otherwise, connect to the randomly-generated service port.
			ports[i] = portMappings[i].ServicePort
		}
	}

	return ports, labels
}
