# Private S3 website quickstart

Run the S3 origin and qURL™ Connector in one network namespace. The origin
listens on `127.0.0.1:8080`; only the Connector provides the access path. Do
not publish port 8080 or 9090, create an origin load balancer, or enable
`ALLOW_NON_LOOPBACK_LISTEN`.

## Prepare one installation

1. Put `index.html` and the site's other objects in a private S3 bucket. Keep
   S3 Block Public Access enabled. Use a non-dotted bucket name and the
   [IAM policy and credential options](README.md#aws-credentials).
2. In the target Slack channel, run `/qurl-admin protect`, choose **Protect
   qURL Connector**, then **S3 static website**. Choose a stable Connector ID
   (these examples use `docs-site`), bucket, AWS region, optional prefix
   (`site` below), index document and deployment environment.
3. Save the generated **share.yaml** unchanged. It contains your server-issued
   owner/resource/routing/knock identities, CRID and serving epoch. Its route
   must point at `http://127.0.0.1:8080`, with `local_ip: 127.0.0.1` and
   `local_port: 8080`. Do not use IDs from a test fixture or another install.
   Keep `connector_id` equal to the chosen `CACHE_CONNECTOR_ID`.
4. Save the separate one-shot enrollment token to an owner-only local file
   named `enrollment-token`. Do not put it in YAML, an environment variable,
   a command argument, or shell history. In a new install directory, use this
   Bash prompt; it refuses to overwrite an existing token file:

   ```bash
   (
     umask 077
     set -C
     read -r -s -p 'Enrollment token: ' token || exit
     printf '\n' >&2
     printf '%s' "$token" > enrollment-token
   )
   ```

5. Use the immutable `ghcr.io/layervai/qurl@sha256:...` reference in the
   matching CLI release's `qurl-image.txt`, and the immutable S3 origin
   reference in the generated install instructions. Use those two complete
   references wherever an example below says `QURL_IMAGE` or `ORIGIN_IMAGE`.
   These examples use the CLI image's `/usr/local/bin/qurl daemon run` runtime;
   do not substitute a standalone `qurl-connector` binary or a `latest` tag.

The examples below use the production Hub default and API origin. For Sandbox
or a custom Hub, use the generated instructions for that environment, including
its explicit Hub host, port and public-key flags. Changing `QURL_ENDPOINT`
alone does not select a different Hub. Enrollment tokens expire; use the newest
install attempt and discard earlier tokens.

Use one durable state volume per Connector. Never mount that state in two
running tasks, pods or containers. Keep it across restarts; losing it requires
a fresh enrollment. None of the examples grants public inbound network access.

## Docker Compose on a Linux host

This example uses an EC2 instance role with bucket access and IMDSv2 hop limit
2. For a different credential provider, add the documented read-only credential
mount or workload identity to **origin**. Do not copy AWS credentials into the
Compose file. Both containers must use the same CPU architecture.

Run from the directory containing your generated `share.yaml` and token file:

```sh
sudo install -d -m 0700 -o 65532 -g 65532 state secrets
sudo install -m 0400 -o 65532 -g 65532 enrollment-token secrets/enrollment-token
sudo chown root:root share.yaml
sudo chmod 0644 share.yaml
```

Save this as `compose.yaml`. Set the two image variables in your shell to the
verified immutable references before running Compose. Replace the bucket,
region and prefix with the same values used during setup.

```yaml
services:
  origin:
    image: "${ORIGIN_IMAGE:?Set the verified S3 origin digest}"
    user: "65532:65532"
    restart: unless-stopped
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    environment:
      S3_BUCKET: example-private-site
      AWS_REGION: us-east-1
      S3_PREFIX: site
      INDEX_DOCUMENT: index.html
      CACHE_CONNECTOR_ID: docs-site
  qurl:
    image: "${QURL_IMAGE:?Set the verified CLI image digest}"
    user: "65532:65532"
    restart: unless-stopped
    read_only: true
    tmpfs: ["/tmp:rw,size=64m"]
    pids_limit: 512
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    entrypoint: [/usr/local/bin/qurl]
    command: [daemon, run, --state-dir, /var/lib/qurl, --headless-config, /etc/qurl/share.yaml, --enrollment-token-file, /run/secrets/qurl/enrollment-token]
    network_mode: service:origin
    depends_on:
      origin:
        condition: service_started
    environment:
      QURL_ENDPOINT: https://api.layerv.ai
    volumes:
      - ./state:/var/lib/qurl
      - ./secrets:/run/secrets/qurl:ro
      - ./share.yaml:/etc/qurl/share.yaml:ro
```

```sh
docker compose config --quiet
docker compose up -d
docker compose logs --tail=50 origin qurl
```

`service_started` orders launch, not readiness. The Connector may report local
connection errors until the origin is listening. If the origin is recreated
or auto-restarts, restart/recreate **qurl** too: it uses the origin's network
namespace. After a Docker daemon restart, check both services. See
[Compose networking](https://docs.docker.com/compose/how-tos/networking/).

After successful enrollment and the smoke below, remove the enrollment-token
flag and its following path from `command`, remove the secrets mount, then run
`docker compose up -d qurl` and repeat the smoke. Keep `state` intact. Only
after that warm start succeeds, delete both local copies of the token.

## ECS/Fargate

Use one Linux Fargate task with `awsvpc` networking. Containers in that task
[share localhost](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/fargate-task-networking.html).
Use a private subnet with outbound access for image pulls, the API, native Hub,
DNS and S3. No origin port mapping or inbound rule is needed. EFS still needs
its own mount-target security group permitting TCP 2049 from the task.

Prepare an execution role for image pulls/logs and a task role for the bucket.
Create a CloudWatch log group `/ecs/qurl-s3-website`. Prepare three EFS access
points on the same file system, with transit encryption and IAM authorization:

| Access point | Contents and POSIX access |
| --- | --- |
| `qurl-state` | Dedicated writable root owned by `65532:65532`; qurl creates its owner-only `state` directory here. |
| `qurl-config` | Generated `share.yaml`, readable by UID 65532; mounted read-only. |
| `qurl-bootstrap` | `enrollment-token`, readable only by UID 65532; mounted read-only. |

The task role needs `elasticfilesystem:ClientMount` for those access points
and `elasticfilesystem:ClientWrite` for state. Scope the EFS policy to this
file system and these access points. Do not grant state access to another
running Connector. A shared EFS access point is not a multi-replica identity.

Save this task definition as `task.json`. Replace every angle-bracket value
with your account's values and both images with the verified digest references.
The region below is the ECS/logging region; `AWS_REGION` is the bucket region.
The CPU/memory pair is a starting example, not a throughput claim.

```json
{
  "family": "qurl-s3-docs-site",
  "networkMode": "awsvpc",
  "requiresCompatibilities": ["FARGATE"],
  "cpu": "512",
  "memory": "1024",
  "runtimePlatform": {"operatingSystemFamily": "LINUX", "cpuArchitecture": "X86_64"},
  "taskRoleArn": "<TASK_ROLE_ARN>",
  "executionRoleArn": "<EXECUTION_ROLE_ARN>",
  "containerDefinitions": [
    {
      "name": "origin", "image": "<ORIGIN_IMAGE>", "user": "65532:65532", "essential": true,
      "linuxParameters": {"capabilities": {"drop": ["ALL"]}},
      "environment": [
        {"name": "S3_BUCKET", "value": "example-private-site"},
        {"name": "AWS_REGION", "value": "us-east-1"},
        {"name": "S3_PREFIX", "value": "site"},
        {"name": "INDEX_DOCUMENT", "value": "index.html"},
        {"name": "CACHE_CONNECTOR_ID", "value": "docs-site"}
      ],
      "logConfiguration": {"logDriver": "awslogs", "options": {"awslogs-group": "/ecs/qurl-s3-website", "awslogs-region": "<ECS_REGION>", "awslogs-stream-prefix": "origin"}}
    },
    {
      "name": "qurl", "image": "<QURL_IMAGE>", "user": "65532:65532", "essential": true,
      "readonlyRootFilesystem": true,
      "entryPoint": ["/usr/local/bin/qurl"],
      "command": ["daemon", "run", "--state-dir", "/var/lib/qurl-volume/state", "--headless-config", "/etc/qurl/share.yaml", "--enrollment-token-file", "/run/secrets/qurl/enrollment-token"],
      "linuxParameters": {"capabilities": {"drop": ["ALL"]}},
      "environment": [{"name": "QURL_ENDPOINT", "value": "https://api.layerv.ai"}],
      "dependsOn": [{"containerName": "origin", "condition": "START"}],
      "mountPoints": [
        {"sourceVolume": "qurl-state", "containerPath": "/var/lib/qurl-volume", "readOnly": false},
        {"sourceVolume": "qurl-config", "containerPath": "/etc/qurl", "readOnly": true},
        {"sourceVolume": "qurl-bootstrap", "containerPath": "/run/secrets/qurl", "readOnly": true}
      ],
      "logConfiguration": {"logDriver": "awslogs", "options": {"awslogs-group": "/ecs/qurl-s3-website", "awslogs-region": "<ECS_REGION>", "awslogs-stream-prefix": "qurl"}}
    }
  ],
  "volumes": [
    {"name": "qurl-state", "efsVolumeConfiguration": {"fileSystemId": "<EFS_ID>", "transitEncryption": "ENABLED", "authorizationConfig": {"accessPointId": "<STATE_ACCESS_POINT_ID>", "iam": "ENABLED"}}},
    {"name": "qurl-config", "efsVolumeConfiguration": {"fileSystemId": "<EFS_ID>", "transitEncryption": "ENABLED", "authorizationConfig": {"accessPointId": "<CONFIG_ACCESS_POINT_ID>", "iam": "ENABLED"}}},
    {"name": "qurl-bootstrap", "efsVolumeConfiguration": {"fileSystemId": "<EFS_ID>", "transitEncryption": "ENABLED", "authorizationConfig": {"accessPointId": "<BOOTSTRAP_ACCESS_POINT_ID>", "iam": "ENABLED"}}}
  ]
}
```

Register the task definition with `aws ecs register-task-definition
--cli-input-json file://task.json`. Create a service with desired count **1**,
minimum healthy percent **0**, maximum percent **100**, and the prepared
subnet/security-group settings. This stops the old task before a replacement
uses the same durable identity. Do not manually run a second copy of the task.
Both containers are essential; losing either replaces the task.

After successful enrollment and the smoke, register a warm-start revision
without the enrollment-token flag/path, bootstrap mount and bootstrap volume.
Update the service, verify the replacement connects from state, repeat the
smoke, then delete the bootstrap token from EFS. Keep the state/config access
points and their task-role permissions.

## Kubernetes

Use one pod, a dedicated PVC, and a service account already configured for
bucket access through EKS Pod Identity or IRSA. Name it `qurl-s3-site` below.
The pod's containers [share localhost](https://kubernetes.io/docs/concepts/services-networking/).
Do not create a Service or Ingress for the origin, use `hostNetwork`, or scale
this single identity above one replica. The cluster needs a default StorageClass
that supports the PVC and the `fsGroup` ownership used below.

Create these objects from your local generated config and token. Set the
intended namespace explicitly in your kubectl context first:

```sh
kubectl create configmap qurl-s3-config --from-file=share.yaml
kubectl create secret generic qurl-s3-bootstrap --from-file=enrollment-token
```

Save the following as `site.yaml`, replace both image values, the bucket,
region and prefix, then apply it with `kubectl apply -f site.yaml`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: qurl-s3-state
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: qurl-s3-site
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: qurl-s3-site
  template:
    metadata:
      labels:
        app: qurl-s3-site
    spec:
      serviceAccountName: qurl-s3-site
      securityContext:
        fsGroup: 65532
        fsGroupChangePolicy: OnRootMismatch
      containers:
        - name: origin
          image: <ORIGIN_IMAGE>
          securityContext:
            runAsUser: 65532
            runAsGroup: 65532
            runAsNonRoot: true
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
            seccompProfile:
              type: RuntimeDefault
          env:
            - {name: S3_BUCKET, value: example-private-site}
            - {name: AWS_REGION, value: us-east-1}
            - {name: S3_PREFIX, value: site}
            - {name: INDEX_DOCUMENT, value: index.html}
            - {name: CACHE_CONNECTOR_ID, value: docs-site}
        - name: qurl
          image: <QURL_IMAGE>
          command: [/usr/local/bin/qurl]
          args: [daemon, run, --state-dir, /var/lib/qurl-volume/state, --headless-config, /etc/qurl/share.yaml, --enrollment-token-file, /run/secrets/qurl/enrollment-token]
          securityContext:
            runAsUser: 65532
            runAsGroup: 65532
            runAsNonRoot: true
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
            seccompProfile:
              type: RuntimeDefault
          env:
            - {name: QURL_ENDPOINT, value: https://api.layerv.ai}
          volumeMounts:
            - {name: tmp, mountPath: /tmp}
            - {name: state, mountPath: /var/lib/qurl-volume}
            - {name: bootstrap, mountPath: /run/secrets/qurl, readOnly: true}
            - {name: config, mountPath: /etc/qurl/share.yaml, subPath: share.yaml, readOnly: true}
      volumes:
        - name: tmp
          emptyDir:
            sizeLimit: 64Mi
        - name: state
          persistentVolumeClaim:
            claimName: qurl-s3-state
        - name: bootstrap
          secret:
            secretName: qurl-s3-bootstrap
            defaultMode: 0440
        - name: config
          configMap:
            name: qurl-s3-config
```

`Recreate` avoids overlapping deployments of the stateful Connector. Do not
force-delete a pod that may still run on an unreachable node; wait for that
node to be fenced before starting a replacement against the same PVC.
`fsGroup` makes the PVC root writable, and qurl creates its private nested
state directory as UID 65532. No HTTP readiness/liveness probe can reach an
origin bound only to pod loopback from the node; do not add a port exposure to
make such a probe work.

After enrollment and the smoke, remove the enrollment-token flag/path,
bootstrap volume mount and bootstrap volume from `site.yaml`. Apply it, wait
for `kubectl rollout status deployment/qurl-s3-site`, verify reconnect and repeat
the smoke. Then delete `qurl-s3-bootstrap` and the local token file. Keep the PVC.

## Verify the protected object

1. Check origin logs for startup/preflight failures and Connector logs for a
   connected route. These are checks of setup, not proof of viewer access.
2. In the same Slack channel, run `/qurl get $docs-site` (substitute the alias
   chosen during setup). Open the returned link through the normal qURL
   browser flow. Complete the access step, then verify the expected unique
   content of your private `index.html` and one nested asset.
3. Request a known missing path through that authenticated site. It must return
   the origin's plain `404`, with no S3 XML body or bucket details.
4. From a different workload on the host/VPC/cluster network, check that the
   origin's workload IP at ports 8080 and 9090 is unreachable. Do not run this
   check from the origin's own namespace, where loopback access is expected.
5. Do the platform's warm-start transition above, mint a **fresh** link, and
   repeat the page/asset check. Do not expect a consumed one-time link to work
   again. Confirm that the S3 objects still cannot be fetched anonymously.

Keep object keys, links, tokens and customer IDs out of shared test reports.
Use the [operator queries](README.md#operator-queries-and-troubleshooting) for
masked failures. A parsed manifest or a successful container start does not
replace this end-to-end check.
