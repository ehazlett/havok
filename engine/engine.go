package engine

import (
	"fmt"
	"regexp"

	"github.com/Sirupsen/logrus"
	"github.com/coreos/go-etcd/etcd"
	"github.com/ehazlett/dockerclient"
)

var log = logrus.New()

type (
	Engine struct {
		dockerUrl    string
		etcdMachines []string
		docker       *dockerclient.DockerClient
		etcdClient   *etcd.Client
		rootDomain   string
		hostIP       string
		nameRegex    string
	}
)

func NewEngine(dockerUrl string, etcdMachines []string, rootDomain string, hostIP string, nameRegex string) *Engine {
	docker, err := dockerclient.NewDockerClient(dockerUrl)
	if err != nil {
		log.Fatalf("Unable to connect to docker: %s", err)
	}
	etcdClient := etcd.NewClient(etcdMachines)
	e := &Engine{
		dockerUrl:    dockerUrl,
		etcdMachines: etcdMachines,
		docker:       docker,
		etcdClient:   etcdClient,
		rootDomain:   rootDomain,
		hostIP:       hostIP,
		nameRegex:    nameRegex,
	}
	return e
}

func (e *Engine) eventHandler(event *dockerclient.Event, args ...interface{}) {
	cnt, err := e.docker.InspectContainer(event.Id)
	if err != nil {
		log.Warn(err)
		return
	}
	name := cnt.Name[1:]
	matched, err := regexp.MatchString(e.nameRegex, name)
	if err != nil {
		log.Errorf("Error matching container: %s", err)
		return
	}
	// if not a match, return immediately
	if !matched {
		return
	}
	host := fmt.Sprintf("%s.%s", cnt.Config.Hostname, e.rootDomain)
	hostKey := fmt.Sprintf("/vulcand/hosts/" + host)
	up := fmt.Sprintf("up-%s", host)
	upKey := fmt.Sprintf("/vulcand/upstreams/%s", up)
	ep := fmt.Sprintf("%s/endpoints", upKey)
	epKey := fmt.Sprintf("%s/%s", ep, name)
	switch event.Status {
	case "start":
		// for now only get the first port for use with etcd since it would
		// be crazy to have multiple endpoints with varying ports
		for _, v := range cnt.NetworkSettings.Ports {
			// check for exposed ports ; if none, report error
			if len(v) == 0 {
				log.WithFields(logrus.Fields{
					"host":      host,
					"container": cnt.Id,
				}).Error("Unable to add endpoint; no ports exposed")
				return
			}
			m := v[0]
			port := m.HostPort
			cntConn := fmt.Sprintf("http://%s:%s", e.hostIP, port)
			log.WithFields(logrus.Fields{
				"host":     host,
				"endpoint": cntConn,
			}).Info("Adding endpoint for host")
			// create key structure in etcd
			_, er := e.etcdClient.Get(hostKey, false, false)
			if er != nil {
				// check for missing key error
				switch er.(*etcd.EtcdError).ErrorCode {
				case 100:
					// key not found ; create
					_, err := e.etcdClient.CreateDir(hostKey, 0)
					if err != nil {
						log.WithFields(logrus.Fields{
							"host":  host,
							"key":   hostKey,
							"error": err,
						}).Error("Error creating host key in etcd")
						return
					}
				default:
					log.WithFields(logrus.Fields{
						"host":  host,
						"key":   hostKey,
						"error": er,
					}).Error("Error checking host key in etcd")
					return
				}
			}
			// set endpoint
			_, err = e.etcdClient.Set(epKey, cntConn, 0)
			if err != nil {
				log.WithFields(logrus.Fields{
					"host":  host,
					"key":   upKey,
					"error": err,
				}).Error("Error creating endpoint in etcd")
				return
			}
			// set location
			locKey := fmt.Sprintf("%s/locations/home/path", hostKey)
			_, err = e.etcdClient.Set(locKey, "/.*", 0)
			if err != nil {
				log.WithFields(logrus.Fields{
					"host":  host,
					"key":   locKey,
					"error": err,
				}).Error("Error creating location in etcd")
				return
			}
			locUpKey := fmt.Sprintf("%s/locations/home/upstream", hostKey)
			_, err = e.etcdClient.Set(locUpKey, up, 0)
			if err != nil {
				log.WithFields(logrus.Fields{
					"host":  host,
					"key":   locKey,
					"error": err,
				}).Error("Error creating location upstream in etcd")
				return
			}
			break
		}
	case "die":
		// remove endpoint
		_, err = e.etcdClient.RawDelete(epKey, true, true)
		if err != nil {
			log.WithFields(logrus.Fields{
				"host":  host,
				"key":   upKey,
				"error": err,
			}).Error("Error removing endpoint from etcd")
			return
		}
		// check for any other endpoints and break if they exist
		r, er := e.etcdClient.Get(ep, true, true)
		if er != nil {
			log.WithFields(logrus.Fields{
				"host":  host,
				"key":   ep,
				"error": er,
			}).Error("Error checking endpoint from etcd")
			return
		}
		// if there are no more nodes, cleanup
		if len(r.Node.Nodes) == 0 {
			// if no more endpoints (all are gone) then remove upstream and host
			_, err = e.etcdClient.RawDelete(upKey, true, true)
			if err != nil {
				log.WithFields(logrus.Fields{
					"host":  host,
					"key":   upKey,
					"error": err,
				}).Error("Error removing upstream from etcd")
			}
			log.WithFields(logrus.Fields{
				"host": host,
			}).Info("Removing host")
			// remove host
			_, err := e.etcdClient.RawDelete(hostKey, true, true)
			if err != nil {
				log.WithFields(logrus.Fields{
					"host":  host,
					"key":   hostKey,
					"error": err,
				}).Error("Error removing host from etcd")
			}
			return
		}
	}
}

func (e *Engine) Run() {
	log.WithFields(logrus.Fields{
		"ip":     e.hostIP,
		"domain": e.rootDomain,
		"docker": e.dockerUrl,
		"etcd":   e.etcdMachines,
	}).Info("Starting Engine")
	// listen for docker events
	e.docker.StartMonitorEvents(e.eventHandler)
}

func (e *Engine) Stop() {
	log.Info("Stopping Engine")
	e.docker.StopAllMonitorEvents()
}
