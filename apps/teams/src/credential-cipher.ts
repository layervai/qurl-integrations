import { DecryptCommand, EncryptCommand, KMSClient } from '@aws-sdk/client-kms';

export interface CredentialCipher {
  encrypt(tenantId: string, plaintext: string): Promise<string>;
  decrypt(tenantId: string, ciphertext: string): Promise<string>;
}

export interface KmsCredentialCipherOptions {
  readonly keyId: string;
  readonly region: string;
  readonly client?: KMSClient;
}

const PREFIX = 'kms:v1:';
const CONTEXT_DOMAIN = 'qurl-teams-tenant-credential';

function context(tenantId: string): Record<string, string> {
  return { domain: CONTEXT_DOMAIN, tenant_id: tenantId, field: 'qurl_api_key' };
}

function encode(value: Uint8Array): string {
  return `${PREFIX}${Buffer.from(value).toString('base64url')}`;
}

function decode(value: string): Uint8Array {
  if (!value.startsWith(PREFIX)) throw new Error('tenant credential is not KMS encrypted');
  const encoded = value.slice(PREFIX.length);
  if (!encoded || !/^[A-Za-z0-9_-]+$/.test(encoded) || encoded.length % 4 === 1) throw new Error('tenant credential ciphertext is malformed');
  const decoded = Buffer.from(encoded, 'base64url');
  if (!decoded.length || decoded.toString('base64url') !== encoded) throw new Error('tenant credential ciphertext is malformed');
  return decoded;
}

export class KmsCredentialCipher implements CredentialCipher {
  readonly #keyId: string;
  readonly #client: KMSClient;

  constructor(options: KmsCredentialCipherOptions) {
    if (!options.keyId.trim() || !options.region.trim()) throw new Error('KMS credential cipher configuration is invalid');
    this.#keyId = options.keyId;
    this.#client = options.client ?? new KMSClient({ region: options.region });
  }

  async encrypt(tenantId: string, plaintext: string): Promise<string> {
    const result = await this.#client.send(new EncryptCommand({
      KeyId: this.#keyId,
      Plaintext: Buffer.from(plaintext, 'utf8'),
      EncryptionContext: context(tenantId),
    }));
    if (!result.CiphertextBlob) throw new Error('KMS returned no tenant credential ciphertext');
    return encode(result.CiphertextBlob);
  }

  async decrypt(tenantId: string, ciphertext: string): Promise<string> {
    const result = await this.#client.send(new DecryptCommand({
      KeyId: this.#keyId,
      CiphertextBlob: decode(ciphertext),
      EncryptionContext: context(tenantId),
    }));
    if (!result.Plaintext) throw new Error('KMS returned no tenant credential plaintext');
    return Buffer.from(result.Plaintext).toString('utf8');
  }
}
