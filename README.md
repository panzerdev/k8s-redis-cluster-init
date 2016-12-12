# k8s-redis-cluster-init

Commandline arguments at startup with ENV names alternatives in `()`
```
  -clusterName string
    	(REDIS_CLUSTER_NAME) - Name of this Redis cluster
  -downAfterMs string
    	(DOWN_AFTER_MS) - Time in ms after which the master is considered down
  -masterQuorum string
    	(REDIS_MASTER_QUORUM) - Number of Sentinels which have to agree that the master is down
  -ns string
    	(NAMESPACE) - Namespace to operate in
  -pathToFile string
    	(PATH_TO_CONFIG_FILE) - Path to config file
  -podIp string
    	(MY_POD_IP) - IP of this pod
  -port string
    	(PORT) - The Port the redis instance will be started on
  -syncHelperHostPort string
    	(SYNC_HELPER_HOST_PORT) - Redis used for distributed synclock host:port
  -sentinelName string
    	(SENTINEL_NAME) - Sentinels service name also used for the Endpoints
  -sentinelPortName string
    	(SENTINEL_PORT_NAME) - Sentinels service portname to look for in Endpoints
```
