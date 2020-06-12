# kbg

kbg (Kube Blue-Green) - _Pronounced "Cabbage"_

<img src="https://github.com/nestorsokil/kbg/raw/master/resources/mascot.png" width="200">

## Installation
### Docker Local

```bash
# assuming local kube is running and kubectx is pointing to it
make install
docker-compose up kbg
```

### Install in Production cluster
TODO

## Workflow
<img src="https://github.com/nestorsokil/kbg/raw/master/resources/kbg.png" width="350">

## Usage

```bash
~/ kubectl create ns test
namespace/test created

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

Update the Nginx version in the sample
```yaml
containers:
  - name: nginx
    image: nginx:1.19.0
```
```bash
~/ kubectl apply -f config/samples/cluster_v1alpha1_bluegreendeployment.yaml -n test
bluegreendeployment.cluster.kbg/myserver configured

~/ kubectl get kbg -n test
NAME       COLOR   STATUS     ACTIVE   BACKUP
myserver   blue    Deploying  2        1

~/ kubectl get all -n test
NAME                       READY   STATUS    RESTARTS   AGE
pod/myserver-blue-6w6st    1/1     Running   0          13m
pod/myserver-green-4nkc4   1/1     Running   0          2m21s
pod/myserver-green-x8dl7   1/1     Running   0          2m37s

NAME                      TYPE       CLUSTER-IP      EXTERNAL-IP   PORT(S)        AGE
service/myserver          NodePort   10.106.146.24   <none>        80:30697/TCP   13m
service/myserver-backup   NodePort   10.107.59.27    <none>        80:31623/TCP   13m

NAME                             DESIRED   CURRENT   READY   AGE
replicaset.apps/myserver-blue    1         1         1       13m
replicaset.apps/myserver-green   2         2         2       2m37s

~/ kubectl get kbg -n test
NAME       COLOR   STATUS    ACTIVE   BACKUP
myserver   green   Nominal   2        1

~/ curl -sD - 127.0.0.1:30697 -o /dev/null | grep Server
Server: nginx/1.19.0
```

Clean up
```bash
~/ kubectl delete kbg --all -n test
bluegreendeployment.cluster.kbg "myserver" deleted

~/ kubectl get all -n test
No resources found in test namespace.
```

## TODOs/Missing Features

- HPA support (untested)
- Default/Validating webhooks (untested)
- Sync mode (hook into admission?) to fail the update if tests fail
- RBAC, tested in "privileged" mode
- Unit/Integration testing, then refactoring

## References

- A big part of this is based on [Google's sample Blue/Green Controller](https://github.com/google/blue-green-deployment-controller/blob/kubebuilder/pkg/controller/bluegreendeployment/controller.go)
- [Kubebuilder book](https://book.kubebuilder.io/)
- [Kubebuilder book v1](https://book-v1.book.kubebuilder.io)
