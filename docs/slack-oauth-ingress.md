# Slack OAuth trusted ingress

Slack OAuth rate limits use the rightmost `X-Forwarded-For` (XFF) address.
That address is trustworthy only when one controlled Application Load Balancer
(ALB) appends it and clients cannot reach the application by another path.
These requirements apply to both account connection (`/oauth/qurl/*`) and app
installation (`/oauth/slack/*`). They apply before enabling either flow.

## Required deployment settings

- Set the ALB attribute `routing.http.xff_header_processing.mode` to `append`.
  `preserve` lets a client supply the trusted value; `remove` makes the app use
  a coarse fallback bucket. Neither mode meets this contract.
- Give ECS tasks private addresses and disable public-IP assignment. Attach
  only the dedicated task security group (SG).
- Allow the container port only from the dedicated ALB SG. No public/private
  CIDR, prefix-list, peering or other SG ingress may bypass the ALB.
- Attach the ALB SG only to that ALB. Another workload with this SG could reach
  tasks and choose its own XFF value.
- Review this boundary before adding a CDN, proxy, service mesh, alternate
  listener, task endpoint, service discovery route or debugging port. The app's
  rightmost-address rule assumes one trusted append hop; it does not validate
  an arbitrary chain of proxies.

The infrastructure module `modules/qurl-webhook-runtime` explicitly sets append
mode, private task networking and ALB-SG-only container ingress. Its
`runtime_options.tftest.hcl` includes `oauth_trusted_alb_ingress` to reject a
change to preserve/remove mode or the declared task ingress boundary. Plan tests
check IaC; they do not detect manual changes to running resources.

## Read-only deployment verification

Use the intended account and region. Confirm the account with
`aws sts get-caller-identity` before checking either Sandbox or production.
Do not send a test OAuth callback, change security groups, or deploy to inspect
this boundary.

1. Describe the ECS service. Check `awsvpcConfiguration.assignPublicIp` is
   `DISABLED`, the expected private subnets and only the dedicated task SG are
   configured, and record every attached target group.
2. Describe each target group and its ALB. Check the ALB's attributes with
   `aws elbv2 describe-load-balancer-attributes`; the XFF processing value must
   be `append` for every ingress ALB.
3. Use `aws ec2 describe-security-groups` for every task SG. Inspect **all**
   inbound rules, including IPv6 and referenced SGs. Only TCP on the container
   port from that ALB SG is allowed. An extra permissive SG adds access even
   when the intended SG is correct.
4. Use `aws ec2 describe-network-interfaces` filtered by ALB SG. Confirm every
   attachment belongs to the expected AWS-managed ALB, not another workload.
5. List and describe all running service tasks and their network interfaces.
   Check their actual SGs match the service configuration and no public IPv4
   association exists. Check all current tasks, not one sample or a desired
   task definition. An inaccessible task or failed API call leaves verification
   incomplete.

Record time, environment, counts and pass/fail results. Keep task IPs, customer
IDs, OAuth codes, state, tokens and credentials out of public reports. Repeat
these checks after network or proxy changes; a past check is not a drift monitor.

## OAuth fallback warning or widespread 429s

Start with ALB append mode and direct-access rules. Inspect the structured
`oauth client ip using fallback` warning's `reason` and `fallback` fields.
Missing or malformed XFF falls back to `RemoteAddr`, then `unknown`; behind the
ALB this can combine users into one bucket. Do not fix this by trusting the
leftmost header or disabling the limiter. Restore the declared proxy boundary
through the normal reviewed deployment path.

Budgets are per process, and clients behind one NAT share a bucket. This
limiter supplements edge controls; it is not a global fleet limit.
