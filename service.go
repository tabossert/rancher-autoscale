package main

import (
  "os"
  "fmt"
  "log"
  "time"
  "strings"
  //"net/http"
  //"io/ioutil"
  //"encoding/json"
  //"golang.org/x/net/websocket"
  "github.com/urfave/cli"
  "github.com/google/cadvisor/client"
  "github.com/google/cadvisor/info/v1"
  "github.com/rancher/go-rancher-metadata/metadata"
  //rclient "github.com/rancher/go-rancher/client"
)

const (
  // Rancher metadata endpoint URL 
  metadataUrl = "http://rancher-metadata.rancher.internal/2015-12-19"
  // frequency at which to poll cAdvisor for metrics
  pollFrequency = 1 * time.Second
  pollService = 15 * time.Second
)

func ServiceCommand() cli.Command {
  return cli.Command{
    Name:  "service",
    Usage: "Autoscale a service",
    ArgsUsage: "<stack/service>",
    Action: ScaleService,
    Flags: []cli.Flag{
      cli.Float64Flag{
        Name:  "cpu",
        Usage: "CPU Usage threshold in percent",
        Value: 80,
      },
      cli.Float64Flag{
        Name:  "mem",
        Usage: "Memory Usage threshold in percent",
        Value: 80,
      },
      cli.DurationFlag{
        Name:  "period",
        Usage: "",
        Value: 60 * time.Second,
      },
      cli.DurationFlag{
        Name:  "warmup",
        Usage: "",
        Value: 60 * time.Second,
      },
      cli.StringFlag{
        Name:  "url",
        Usage: "Rancher API URL",
        Value: os.Getenv("CATTLE_URL"),
      },
      cli.StringFlag{
        Name:  "access-key",
        Usage: "Rancher Access Key",
        Value: os.Getenv("CATTLE_ACCESS_KEY"),
      },
      cli.StringFlag{
        Name:  "secret-key",
        Usage: "Rancher Secret Key",
        Value: os.Getenv("CATTLE_SECRET_KEY"),
      },
    },
  }
}

type AutoscaleClient struct {
  // configuration argument
  StackName      string
  Service        metadata.Service
  
  // configuration parameters
  CpuThreshold   float64
  MemThreshold   float64
  Warmup         time.Duration
  Period         time.Duration

  mClient        *metadata.Client
  mContainers    []metadata.Container
  mHosts         []metadata.Host
  CContainers    []v1.ContainerInfo
  ContainerHosts map[string]metadata.Host
}

func NewAutoscaleClient(c *cli.Context) *AutoscaleClient {
  stackservice := c.Args().First()
  if stackservice == "" {
    cli.ShowCommandHelp(c, "service")
    os.Exit(1)
  }

  tokens := strings.Split(stackservice, "/")
  stackName := tokens[0]
  serviceName := tokens[1]

  mclient := metadata.NewClient(metadataUrl)
  
  service, err := mclient.GetServiceByName(stackName, serviceName)
  if err != nil {
    log.Fatalln(err)
  }
  fmt.Println("Service:", service.Name)

  rcontainers, err := mclient.GetServiceContainers(serviceName, stackName)
  if err != nil {
    log.Fatalln(err)
  }
  fmt.Println("Containers:")
  for _, container := range rcontainers {
    fmt.Println(" ", container.Name)
  }

  // get rancher hosts
  rhosts, err := mclient.GetContainerHosts(rcontainers)
  if err != nil {
    log.Fatalln(err)
  }
  fmt.Println("Rancher Hosts:")
  for _, host := range rhosts {
    fmt.Println(" ", host.Name)
  }

  client := &AutoscaleClient{
    StackName: stackName,
    Service: service,
    CpuThreshold: c.Float64("cpu"),
    MemThreshold: c.Float64("mem"),
    Warmup: c.Duration("warmup"),
    Period: c.Duration("period"),
    mClient: mclient,
    mContainers: rcontainers,
    mHosts: rhosts,
  }

  // get cadvisor containers
  if err := client.GetCadvisorContainers(rcontainers, rhosts); err != nil {
    log.Fatalln(err)
  }

  return client
}

func ScaleService(c *cli.Context) error {
  NewAutoscaleClient(c)
  return nil
}

func (c *AutoscaleClient) GetCadvisorContainers(rancherContainers []metadata.Container, hosts []metadata.Host) error {
  c.ContainerHosts = make(map[string]metadata.Host)
  var cinfo []v1.ContainerInfo

  metrics := make(chan v1.ContainerInfo)
  done := make(chan bool)
  defer close(metrics)
  defer close(done)

  for _, host := range hosts {
    address := "http://" + host.AgentIP + ":9244/"
    cli, err := client.NewClient(address)
    if err != nil {
      return err
    }

    containers, err := cli.AllDockerContainers(&v1.ContainerInfoRequest{ NumStats: 0 })
    if err != nil {
      return err
    }

    for _, container := range containers {
      for _, rancherContainer := range rancherContainers {
        if rancherContainer.Name == container.Labels["io.rancher.container.name"] {
          cinfo = append(cinfo, container)
          c.ContainerHosts[container.Id] = host
          go PollContinuously(container.Id, host.AgentIP, metrics, done)

          // spread out the requests evenly
          time.Sleep(time.Duration(int(pollFrequency) / c.Service.Scale))
          break
        }
      }
    }
  }

  fmt.Println("cAdvisor Containers:")
  for _, container := range cinfo {
    fmt.Println(" ", container.Name)
  }
  c.CContainers = cinfo

  fmt.Printf("Monitoring service '%s' in stack '%s'\n", c.Service.Name, c.StackName)
  go Process(metrics, done)

  return c.PollService(done)
}

// indefinitely poll for service scale changes
func (c *AutoscaleClient) PollService(done chan<- bool) error {
  for {
    time.Sleep(pollService)

    service, err := c.mClient.GetServiceByName(c.StackName, c.Service.Name)
    if err != nil {
      return err
    }

    // if the service is scaled up/down, we accomplished our goal
    if service.Scale > c.Service.Scale {
      fmt.Printf("Detected scale up: %d -> %d\n", c.Service.Scale, service.Scale)
      done<-true
      fmt.Printf("Waiting %v for container to warm up.\n", c.Warmup)
      time.Sleep(c.Warmup)
      fmt.Println("Exiting")
      return nil

    } else if service.Scale < c.Service.Scale {
      fmt.Printf("Detected scale down: %d -> %d\n", c.Service.Scale, service.Scale)
      done<-true
      // maybe we need to cool down for certain types of software?
      fmt.Println("Exiting")
      return nil
    }
  }
  return nil
}

// process incoming metrics
func Process(metrics <-chan v1.ContainerInfo, done chan bool) {
  total := 0
  for {
    select {
    case metric := <-metrics:
      if total += 1; total % 100 == 0 {
        fmt.Printf("Collected %d container metrics\n", total)
        fmt.Println(metric)
      }
    case <-done:
      done<-true
      fmt.Printf("Draining metrics")
      ticks := 0
      for _ = range time.Tick(100 * time.Millisecond) {
        for {
          select {
          case <-metrics:
            log.Printf("Drained", metrics)
          default:
            break
          }
        }
        if ticks += 1; ticks == 10 {
          break
        }
      }
      fmt.Printf("Stopped processing all metrics")
      return
    }
  }
}

// poll cAdvisor continuously for container metrics
func PollContinuously(containerId string, hostIp string, metrics chan<- v1.ContainerInfo, done chan bool) {
  address := "http://" + hostIp + ":9244/"
  cli, err := client.NewClient(address)
  if err != nil {
    log.Fatalln(err)
  }

  start := time.Now()
  for {
    select {
    case <-done:
      done<-true
      fmt.Printf("Stopped collecting metrics for container %s", containerId)
      return
    default:
    }
    time.Sleep(pollFrequency)

    newStart := time.Now()
    info, err := cli.DockerContainer(containerId, &v1.ContainerInfoRequest{
      Start: start,
    })
    if err != nil {
      log.Fatalln(err)
    }

    start = newStart
    metrics <- info
  }
}


  //  curl -s -u $CATTLE_ACCESS_KEY:$CATTLE_SECRET_KEY $CATTLE_URL/projects | jq -r .data[].id
  /*client, err := rclient.NewRancherClient(&rclient.ClientOpts{
    Url:       c.String("url"),
    AccessKey: c.String("access-key"),
    SecretKey: c.String("secret-key"),
  })
  if err != nil {
    log.Fatalln(err)
  }

  serviceFilter := make(map[string]interface{})
  serviceFilter["name"] = serviceName
  serviceCollection, err := client.Service.List(&rclient.ListOpts{
    Filters: serviceFilter,
  })
  if len(serviceCollection.Data) > 1 {
    log.Fatalln("Service name wasn't unique:", serviceName)
  }
  service := serviceCollection.Data[0]
  statsUrl := fmt.Sprintf("%s/projects/%s/services/%s/containerstats", c.String("url"), service.AccountId, service.Id)
  fmt.Println(statsUrl)
  
  // get stats
  resp, err := http.Get(statsUrl)
  if err != nil {
    log.Fatalln(err)
  }
  defer resp.Body.Close()

  body, err := ioutil.ReadAll(resp.Body)
  if err != nil {
    log.Fatalln(err)
  }

  kv := make(map[string]interface{})
  if err := json.Unmarshal(body, &kv); err != nil {
    log.Fatalln(err)
  }

  wsendpoint := kv["url"].(string) + "?token=" + kv["token"].(string)
  ws, err := websocket.Dial(wsendpoint, "", statsUrl)
  if err != nil {
    log.Fatalln(err)
  }
  defer ws.Close()

  var msg = make([]byte, 65536)
  var n int
  if n, err = ws.Read(msg); err != nil {
      log.Fatal(err)
  }
  fmt.Printf("Received: %s.\n", msg[:n])


  os.Exit(0)*/
