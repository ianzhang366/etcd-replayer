# `load-simulator`

```
Usage of load-simulator:
  -clean
    	only do clean up operation
  -concurrent int
    	number of concurrent clients (default 10)
  -duration int
    	duration for running this test, in second (default 10)
  -kubeconfig string
    	absolute path to the kubeconfig file (default "/Users/ianzhang/.kube/config")
  -pprof
    	enable pprof or not
  -template string
    	path to the template file, default is ./testdata/manifestwork-template.yaml (default "./testdata/manifestwork-template.yaml")
  -update
    	do continous update after creation (default true)
```

## Behaviour
`load-simulator` will apply `template` to k8s cluster(pointed by `kubeconfig`) in the following manner.

Open `concurrent` connections, and create or update the `template` every `interval` (default is 5 milliseconds).


**Note: your local env, such as your MACBook, might not have enough resource to run this with 1000 connections. You might want to use a large EC2 instance.**


## Debug
You can use `lsof -i | grep main` to confirm if there's expected connection opened on your manchine.

In addition, if you have performance concern over this, you can use the `pprof` flag to enable the golang pprof.
