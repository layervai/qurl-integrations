import { isChannelAlias } from './alias.js';
import { UserFacingError } from './user-facing-error.js';

const slugPattern = /^[a-z][a-z0-9-]{1,62}[a-z0-9]$/;
const servicePattern = /^[A-Za-z0-9_-]{1,64}$/;

export type TunnelEnvironment = 'docker' | 'compose' | 'ecs-fargate' | 'kubernetes';
export interface TunnelInstallArgs { readonly slug: string; readonly alias: string; readonly environment: TunnelEnvironment; readonly port: number; readonly service?: string; readonly image: string; readonly bootstrapKey: string; }

export function validateTunnelImageRef(image: string): void { if (image && !/^[A-Za-z0-9./:@_-]+$/.test(image)) throw new UserFacingError('invalid connector image reference'); }
export function normalizeTunnelEnvironment(environment: string): TunnelEnvironment {
  const value = environment.trim().toLowerCase();
  if (value === '' || value === 'docker') return 'docker';
  if (value === 'compose' || value === 'docker-compose') return 'compose';
  if (value === 'ecs-fargate') return 'ecs-fargate';
  if (value === 'kubernetes') return 'kubernetes';
  throw new UserFacingError('invalid connector environment');
}
export function validateTunnelSlug(slug: string): void { if (!slugPattern.test(slug)) throw new UserFacingError('connector id must be 3-64 lowercase letters, numbers, or hyphens'); }
export function validateTunnelService(service: string): void { if (service && !servicePattern.test(service)) throw new UserFacingError('connector service is invalid'); }

function shellQuote(value: string): string { return `'${value.replaceAll("'", `'"'"'`)}'`; }
export function renderTunnelInstallMessage(args: TunnelInstallArgs): string {
  validateTunnelSlug(args.slug);
  if (!isChannelAlias(args.alias)) throw new UserFacingError('connector alias is invalid');
  if (!Number.isInteger(args.port) || args.port < 1 || args.port > 65_535) throw new UserFacingError('connector port is invalid');
  validateTunnelImageRef(args.image);
  validateTunnelService(args.service ?? '');
  const image = args.image || 'qurl/connector:configured-by-deployment';
  switch (args.environment) {
    case 'compose': return `Docker Compose service \`${args.service || 'web'}\`:\n\`\`\`yaml\nenvironment:\n  QURL_BOOTSTRAP_KEY: ${JSON.stringify(args.bootstrapKey)}\n  QURL_CONNECTOR_ID: ${JSON.stringify(args.slug)}\n  QURL_LOCAL_PORT: ${JSON.stringify(String(args.port))}\nimage: ${JSON.stringify(image)}\n\`\`\``;
    case 'ecs-fargate': return `ECS/Fargate task-definition fields:\n\`\`\`json\n${JSON.stringify({
      image,
      environment: [
        { name: 'QURL_BOOTSTRAP_KEY', value: args.bootstrapKey },
        { name: 'QURL_CONNECTOR_ID', value: args.slug },
        { name: 'QURL_LOCAL_PORT', value: String(args.port) },
      ],
    }, null, 2)}\n\`\`\``;
    case 'kubernetes': return `Apply a Secret and Deployment with the connector configuration:\n\`\`\`yaml\napiVersion: v1\nkind: Secret\nmetadata:\n  name: qurl-${args.slug}-bootstrap\nstringData:\n  QURL_BOOTSTRAP_KEY: ${JSON.stringify(args.bootstrapKey)}\n---\napiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n        - name: ${args.slug}\n          image: ${JSON.stringify(image)}\n          env:\n            - name: QURL_BOOTSTRAP_KEY\n              valueFrom:\n                secretKeyRef:\n                  name: qurl-${args.slug}-bootstrap\n                  key: QURL_BOOTSTRAP_KEY\n            - name: QURL_CONNECTOR_ID\n              value: ${JSON.stringify(args.slug)}\n            - name: QURL_LOCAL_PORT\n              value: ${JSON.stringify(String(args.port))}\n\`\`\`\nStore the Secret securely and remove this one-time bootstrap key after the connector enrolls.`;
    default: return `Run the connector:\n\`\`\`bash\ndocker run -d --restart unless-stopped -e QURL_BOOTSTRAP_KEY=${shellQuote(args.bootstrapKey)} -e QURL_CONNECTOR_ID=${shellQuote(args.slug)} -e QURL_LOCAL_PORT=${args.port} ${shellQuote(image)}\n\`\`\``;
  }
}
