import { createCipheriv, createDecipheriv, randomBytes } from 'node:crypto';
import { DecryptCommand, GenerateDataKeyCommand, KMSClient } from '@aws-sdk/client-kms';

export interface CredentialCipher {
  encrypt(tenantId: string, plaintext: string): Promise<string>;
  decrypt(tenantId: string, ciphertext: string): Promise<string>;
}

export interface KmsCredentialCipherOptions {
  readonly keyId: string;
  readonly region: string;
  readonly client?: KMSClient;
}

const PREFIX = 'kms:v2:';
const CONTEXT_DOMAIN = 'qurl-teams-tenant-credential';

function context(tenantId: string): Record<string, string> {
  return { domain: CONTEXT_DOMAIN, tenant_id: tenantId, field: 'qurl_api_key' };
}

function encode(value: Uint8Array): string {
  return Buffer.from(value).toString('base64url');
}

function decode(encoded: string): Buffer {
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
    // Match Slack's envelope encryption and the shared KMS endpoint policy.
    const result = await this.#client.send(new GenerateDataKeyCommand({
      KeyId: this.#keyId,
      KeySpec: 'AES_256',
      EncryptionContext: context(tenantId),
    }));
    try {
      if (result.Plaintext?.length !== 32 || !result.CiphertextBlob?.length) throw new Error('KMS returned an invalid tenant data key');
      const nonce = randomBytes(12);
      const cipher = createCipheriv('aes-256-gcm', result.Plaintext, nonce);
      cipher.setAAD(Buffer.from(JSON.stringify(context(tenantId))));
      const ciphertext = Buffer.concat([cipher.update(plaintext, 'utf8'), cipher.final()]);
      return PREFIX + [result.CiphertextBlob, nonce, cipher.getAuthTag(), ciphertext].map(encode).join('.');
    } finally {
      result.Plaintext?.fill(0);
    }
  }

  async decrypt(tenantId: string, ciphertext: string): Promise<string> {
    if (!ciphertext.startsWith(PREFIX)) throw new Error('tenant credential is not KMS encrypted');
    const parts = ciphertext.slice(PREFIX.length).split('.');
    if (parts.length !== 4) throw new Error('tenant credential ciphertext is malformed');
    const [wrappedKey, nonce, tag, payload] = parts.map(decode);
    if (!wrappedKey || nonce?.length !== 12 || tag?.length !== 16 || !payload) throw new Error('tenant credential ciphertext is malformed');
    const result = await this.#client.send(new DecryptCommand({
      KeyId: this.#keyId,
      CiphertextBlob: wrappedKey,
      EncryptionContext: context(tenantId),
    }));
    try {
      if (result.Plaintext?.length !== 32) throw new Error('KMS returned an invalid tenant data key');
      const decipher = createDecipheriv('aes-256-gcm', result.Plaintext, nonce);
      decipher.setAAD(Buffer.from(JSON.stringify(context(tenantId))));
      decipher.setAuthTag(tag);
      return Buffer.concat([decipher.update(payload), decipher.final()]).toString('utf8');
    } finally {
      result.Plaintext?.fill(0);
    }
  }
}
