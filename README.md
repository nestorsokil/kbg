# kbg

kbg (Kube Blue-Green) - _Pronounced "Cabbage"_

<img src="https://github.com/nestorsokil/kbg/raw/master/resources/mascot.png" width="200">

## Workflow
<img src="https://github.com/nestorsokil/kbg/raw/master/resources/kbg.png" width="350">

## Installation
TODO

## Usage

```bash
~/ kubectl apply -f config/samples/cluster_v1alpha1_bluegreendeployment.yaml -n test
bluegreendeployment.cluster.kbg/myserver created

~/ kubectl get all -n test
NAME                       READY   STATUS    RESTARTS   AGE
pod/myserver-blue-6w6st    1/1     Running   0          34s
pod/myserver-blue-q4w2v    1/1     Running   0          34s
pod/myserver-green-qjfcc   1/1     Running   0          34s

NAME                      TYPE       CLUSTER-IP      EXTERNAL-IP   PORT(S)        AGE
service/myserver          NodePort   10.106.146.24   <none>        80:30697/TCP   34s
service/myserver-backup   NodePort   10.107.59.27    <none>        80:31623/TCP   34s

NAME                             DESIRED   CURRENT   READY   AGE
replicaset.apps/myserver-blue    2         2         2       34s
replicaset.apps/myserver-green   1         1         1       34s

~/ kubectl get kbg -n test
NAME       COLOR   STATUS    ACTIVE   BACKUP
myserver   blue    Nominal   2        1
```
