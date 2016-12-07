package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"gopkg.in/redis.v5"
	"io/ioutil"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/fields"
	"k8s.io/client-go/rest"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	keyRedisSentinelServiceHost            = "redis.sentinel.service.host.name"
	keyRedisSentinelServicePort            = "redis.sentinel.service.port.name"
	keyRedisSentinelClusterName            = "redis.sentinel.cluster.name"
	keyRedisSentinelClusterQuorum          = "redis.sentinel.cluster.quorum"
	keyRedisSentinelClusterDownAfterMs     = "redis.sentinel.cluster.down.after.ms"
	keyRedisSentinelClusterParallelSync    = "redis.sentinel.cluster.parallel.syncs"
	keyRedisSentinelClusterFailoverTimeout = "redis.sentinel.cluster.failover.timeout"
)

// Pod values
var podIp = flag.String("podIp", os.Getenv("MY_POD_IP"), "(MY_POD_IP) - IP of this pod")
var nameSpace = flag.String("ns", os.Getenv("NAMESPACE"), "(NAMESPACE) - Namespace to operate in")
var port = flag.String("port", os.Getenv("PORT"), "(PORT) - The Port the redis instance will be started on")

// Helper redis for getting a distributed lock
var syncRedisHostPort = flag.String("syncHelperHostPort", os.Getenv("SYNC_HELPER_HOST_PORT"), "(SYNC_HELPER_HOST_PORT) - Redis used for distributed synclock host:port")

// Sentinel service values
var seName = flag.String("sentinelName", os.Getenv("SENTINEL_NAME"), "(SENTINEL_NAME) - Sentinels service name also used for the Endpoints")
var sePortName = flag.String("sentinelPortName", os.Getenv("SENTINEL_PORT_NAME"), "(SENTINEL_PORT_NAME) - Sentinels service portname to look for in Endpoints")

// Sentinel values for configuring the cluster
var redisClusterName = flag.String("clusterName", os.Getenv("REDIS_CLUSTER_NAME"), "(REDIS_CLUSTER_NAME) - Name of this Redis cluster")
var redisMasterQuorum = flag.String("masterQuorum", os.Getenv("REDIS_MASTER_QUORUM"), "(REDIS_MASTER_QUORUM) - Number of Sentinels which have to agree that the master is down")
var downAfter = flag.String("downAfterMs", os.Getenv("DOWN_AFTER_MS"), "(DOWN_AFTER_MS) - Time in ms after which the master is considered down")
var parallelSync = flag.String("parallelSync", os.Getenv("PARALLEL_SYNC"), "(PARALLEL_SYNC) - Nr of parallel sync of slaves")
var failoverTimeout = flag.String("failoverTimeout", os.Getenv("FAILOVER_TIMEOUT"), "(FAILOVER_TIMEOUT) - Failover timeout")

// Paths to raw config file and optionally the annotation file containing ALL the config values
var confFilePath = flag.String("pathToFile", os.Getenv("PATH_TO_CONFIG_FILE"), "(PATH_TO_CONFIG_FILE) - Path to config file")
var configValuesFile = flag.String("pathToConfigFile", os.Getenv("PATH_TO_CONFIG_VALUE_FILE"), "(PATH_TO_CONFIG_VALUE_FILE) - Path to config values from annotation file")

var redisHelperKey string

func main() {
	log.Println("Starting up redis watcher")
	flag.Parse()

	if *configValuesFile != "" {
		log.Println("Filepath found for config values")
		getValuesFromFile()
	}
	flag.VisitAll(func(f *flag.Flag) {
		log.Printf("Flag \n Name:\t\t%v \n Value:\t\t%v \n DefaultVal:\t%v \n------------------- \n", f.Name, f.Value, f.DefValue)
	})

	// key used for lock in sync-redis
	redisHelperKey = *redisClusterName + *nameSpace

	sleepForFailover()

	syncMasterRedis := redis.NewClient(&redis.Options{
		Addr: *syncRedisHostPort,
	})
	if err := syncMasterRedis.Ping().Err(); err != nil {
		log.Fatalln("Not possible to connect to sync master", err)
	}

	for {
		r := syncMasterRedis.Get(redisHelperKey)
		val := r.Val()
		log.Printf("%v - Val: %v - Result: (%+v)\n", redisHelperKey, val, r)
		if val == "" {
			valToSet := fmt.Sprint("I am working now ", *podIp)
			sr := syncMasterRedis.Set(redisHelperKey, valToSet, time.Second)
			if err := sr.Err(); err != nil {
				log.Println("Set of Key:", redisHelperKey, "with Value:", valToSet, "failed", err)
				continue
			}
			gr := syncMasterRedis.Get(redisHelperKey)
			if gr.Val() != valToSet {
				log.Println("I wasn't fast enough to claim lock", gr)
				continue
			}
			log.Println("Set of Key:", redisHelperKey, "success! Val:", gr.Val())
			break
		}
		time.Sleep(time.Duration(rand.Int31n(800)) * time.Millisecond)
	}

	doneC := make(chan bool)
	go func() {
		tick := time.NewTicker(600 * time.Millisecond)
		for {
			select {
			case <-tick.C:
				sr := syncMasterRedis.Set(redisHelperKey, fmt.Sprint("I am working now ", *podIp), time.Second)
				if err := sr.Err(); err != nil {
					log.Println("Set of Key:", redisHelperKey, "failed", err)
				} else {
					log.Println("Extended time of Key:", redisHelperKey)
				}
			case extend := <-doneC:
				tick.Stop()
				log.Println("Done Signal received from Master setup:", extend)
				if extend {
					sr := syncMasterRedis.Set(redisHelperKey, "Master is set now let's let the slaves wait a bit", 10*time.Second)
					if err := sr.Err(); err != nil {
						log.Println("Set of master-claimed failed", err)
					} else {
						log.Println("Extended time of", redisHelperKey)
					}
				} else {
					dr := syncMasterRedis.Del(redisHelperKey)
					log.Printf("Del of key %v - Result: (%+v)\n", redisHelperKey, dr)
				}
				syncMasterRedis.Close()
				doneC <- true
				return
			}
		}
	}()

	clientset := getClient()

	eAddr, ePorts := getEndpoints(clientset)
	log.Printf("Endpoint Addresses: %+v - Ports: %+v", eAddr, ePorts)

	var seEndpointPort *v1.EndpointPort
	for _, v := range ePorts {
		if v.Name == *sePortName {
			seEndpointPort = &v
			break
		}
	}
	if seEndpointPort == nil {
		log.Fatalf("No Port found for Sentinel by the name of %v in %+v", *sePortName, ePorts)
	}

	masterFound := false
	var masterVal []string

	// check if there is a master in any sentinel
	for i, v := range eAddr {
		sentinelHostPort := fmt.Sprintf("%v:%v", v.IP, seEndpointPort.Port)
		log.Println("Nr:", i+1, "Sentinel to check for master", sentinelHostPort)

		sentinelClient := redis.NewClient(&redis.Options{
			Addr:       sentinelHostPort,
			MaxRetries: 10,
		})
		if r := sentinelClient.Ping(); r.Err() != nil {
			log.Fatal("Error on PING Sentinel", *r)
		}
		log.Println("Sentinel responding")

		// commands
		cmd := redis.NewStringSliceCmd("SENTINEL", "get-master-addr-by-name", *redisClusterName)
		sentinelClient.Process(cmd)
		if cmd.Err() == nil {
			pm := cmd.Val()
			masterHost := pm[0]
			masterPort := pm[1]
			potentialMaster := masterHost + ":" + masterPort
			log.Println("Potential Master found", potentialMaster)

			masterTestClient := redis.NewClient(&redis.Options{
				Addr:       potentialMaster,
				MaxRetries: 10,
			})
			if r := masterTestClient.Ping(); r.Err() != nil {
				log.Println("Error on PING potential master", *r)

				rmCmd := redis.NewStatusCmd("SENTINEL", "REMOVE", *redisClusterName)
				sentinelClient.Process(rmCmd)
				if rmCmd.Err() != nil {
					log.Fatalf("REMOVE Master went wrong... --- %+v", rmCmd)
				}
				log.Println("Master removed from Sentinel", sentinelHostPort, *redisClusterName)
			} else {
				log.Println("Master", potentialMaster, "Could be pinged")
				masterVal = pm
				masterFound = true
			}
			masterTestClient.Close()
			sentinelClient.Close()
		} else {
			log.Println("No Master found in Sentinel", sentinelHostPort)
			sentinelClient.Close()
		}
	}

	if masterFound {
		masterHost := masterVal[0]
		masterPort := masterVal[1]

		content, err := ioutil.ReadFile(*confFilePath)
		if err != nil {
			log.Fatal("File read problem", err)
		}
		log.Printf("Raw config file:\n%s", string(content))

		f, err := os.OpenFile(*confFilePath, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			log.Fatal("Error on file open", err)
		}

		_, err = f.WriteString(fmt.Sprintln("slaveof", masterHost, masterPort))
		if err != nil {
			log.Println("Err on write", err)
		}
		f.Close()

		content, err = ioutil.ReadFile(*confFilePath)
		if err != nil {
			log.Fatal("File read problem", err)
		}
		log.Printf("Modified config file:\n%s", string(content))
		log.Println("Things went well so let's start that slave up!")
		doneC <- false
	} else {
		log.Println("Starting to tell Sentinels about me beeing the master:", *redisClusterName)
		for i, v := range eAddr {
			sentinelHostPort := fmt.Sprintf("%v:%v", v.IP, seEndpointPort.Port)
			log.Println("Sentinel:", i+1, "at:", sentinelHostPort, "beeing called")

			seClient := redis.NewClient(&redis.Options{
				Addr: sentinelHostPort,
			})
			r := seClient.Ping()
			if r.Err() != nil {
				log.Fatal("Error on PING Sentinel", *r)
			}
			log.Println("Sentinel responded")

			sCmd := redis.NewStatusCmd("SENTINEL", "MONITOR", *redisClusterName, *podIp, *port, *redisMasterQuorum)
			seClient.Process(sCmd)
			if sCmd.Err() != nil {
				log.Fatalf("MONITOR Master went wrong... --- %+v", sCmd)
			}
			log.Println("Sentiel MONITOR done:", sCmd)

			sCmd = redis.NewStatusCmd("SENTINEL", "SET", *redisClusterName, "down-after-milliseconds", *downAfter)
			seClient.Process(sCmd)
			if sCmd.Err() != nil {
				log.Fatalf("SET down-after-milliseconds went wrong... --- %+v", sCmd)
			}
			log.Println("Sentiel SET down-after-milliseconds done:", sCmd)

			sCmd = redis.NewStatusCmd("SENTINEL", "SET", *redisClusterName, "parallel-syncs", *parallelSync)
			seClient.Process(sCmd)
			if sCmd.Err() != nil {
				log.Fatalf("SET parallel-syncs went wrong... --- %+v", sCmd)
			}
			log.Println("Sentiel SET parallel-syncs done:", sCmd)

			sCmd = redis.NewStatusCmd("SENTINEL", "SET", *redisClusterName, "failover-timeout", *failoverTimeout)
			seClient.Process(sCmd)
			if sCmd.Err() != nil {
				log.Fatalf("SET failover-timeout went wrong... --- %+v", sCmd)
			}
			log.Println("Sentiel failover-timeout set:", sCmd)

			seClient.Close()
			log.Println("I am the master now! Wohooo!")
		}
		doneC <- true
	}
	<-doneC
	log.Println("All good lets go fire up this redis!")
}

func getValuesFromFile() {
	content, err := ioutil.ReadFile(*configValuesFile)
	if err != nil {
		log.Fatal("File read problem", err)
	}
	s := bufio.NewScanner(bytes.NewReader(content))
	mp := map[string]string{}
	for s.Scan() {
		line := s.Text()
		tmpSlice := strings.SplitN(line, "=", 2)
		mp[tmpSlice[0]] = strings.Replace(tmpSlice[1], "\"", "", -1)
	}
	if err = s.Err(); err != nil {
		log.Fatal("Scanner error", err)
	}

	if *seName = mp[keyRedisSentinelServiceHost]; *seName == "" {
		log.Fatalln("Missing value for", keyRedisSentinelServiceHost)
	}
	if *sePortName = mp[keyRedisSentinelServicePort]; *sePortName == "" {
		log.Fatalln("Missing value for", keyRedisSentinelServicePort)
	}
	if *redisClusterName = mp[keyRedisSentinelClusterName]; *redisClusterName == "" {
		log.Fatalln("Missing value for", keyRedisSentinelClusterName)
	}
	if *redisMasterQuorum = mp[keyRedisSentinelClusterQuorum]; *redisMasterQuorum == "" {
		log.Fatalln("Missing value for", keyRedisSentinelClusterQuorum)
	}
	if *downAfter = mp[keyRedisSentinelClusterDownAfterMs]; *downAfter == "" {
		log.Fatalln("Missing value for", keyRedisSentinelClusterDownAfterMs)
	}
	if *parallelSync = mp[keyRedisSentinelClusterParallelSync]; *parallelSync == "" {
		log.Fatalln("Missing value for", keyRedisSentinelClusterParallelSync)
	}
	if *failoverTimeout = mp[keyRedisSentinelClusterFailoverTimeout]; *failoverTimeout == "" {
		log.Fatalln("Missing value for", keyRedisSentinelClusterFailoverTimeout)
	}
	log.Println("Getting values from annotation config file worked out fine...")
}

func getClient() *kubernetes.Clientset {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal("Create InClusterConfig", err.Error())
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("Create API client for Config", err.Error())
	}
	return clientset
}

func getEndpoints(clientset *kubernetes.Clientset) ([]v1.EndpointAddress, []v1.EndpointPort) {
	endpoints, err := clientset.Core().
		Endpoints(*nameSpace).
		List(v1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("metadata.name", *seName).String(),
		})
	if err != nil {
		log.Fatalln("Endpoint request failed", err.Error())
	}
	if len(endpoints.Items) != 1 || len(endpoints.Items[0].Subsets) != 1 {
		log.Fatalf("Something wrong with the result Endpoints found: %+v", endpoints)
	}
	subs := endpoints.Items[0].Subsets[0]
	if len(subs.Addresses) == 0 {
		log.Fatalf("There were no ready Addresses found for this Endpoin: %+v ", endpoints)
	}
	return subs.Addresses, subs.Ports
}

func sleepForFailover() {
	dA, err := strconv.Atoi(*downAfter)
	if err != nil {
		log.Fatalln("downAfter is no number", *downAfter)
	}
	timeToSleep := time.Duration(2*dA) * time.Millisecond
	log.Println("TimeToSleep to make sure the Master has fallen over for sure", timeToSleep)
	time.Sleep(timeToSleep)
}
