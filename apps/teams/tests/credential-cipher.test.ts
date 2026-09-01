import { describe, expect, it } from 'vitest';
import type { KMSClient } from '@aws-sdk/client-kms';
import { KmsCredentialCipher } from '../src/credential-cipher.js';

describe('KMS credential cipher', () => {
  it('round-trips credentials and binds both operations to the tenant context', async () => {
    const requests: Array<Record<string, unknown>> = [];
    const client = {
      send: async (command: { readonly input: Record<string, unknown> }) => {
        requests.push(command.input);
        if ('Plaintext' in command.input) return { CiphertextBlob: command.input.Plaintext };
        return { Plaintext: command.input.CiphertextBlob };
      },
    } as unknown as KMSClient;
    const cipher = new KmsCredentialCipher({ keyId: 'arn:aws:kms:us-east-1:123:key/test', region: 'us-east-1', client });

    const encrypted = await cipher.encrypt('tenant-a', 'secret-api-key');
    await expect(cipher.decrypt('tenant-a', encrypted)).resolves.toBe('secret-api-key');
    expect(encrypted.startsWith('kms:v1:')).toBe(true);
    expect(requests).toHaveLength(2);
    expect(requests[0]?.KeyId).toBe('arn:aws:kms:us-east-1:123:key/test');
    expect(requests[0]?.EncryptionContext).toEqual({ domain: 'qurl-teams-tenant-credential', tenant_id: 'tenant-a', field: 'qurl_api_key' });
    expect(requests[1]?.EncryptionContext).toEqual(requests[0]?.EncryptionContext);
  });

  it('rejects plaintext or malformed stored credentials', async () => {
    const client = { send: async () => ({ Plaintext: new Uint8Array([1]) }) } as unknown as KMSClient;
    const cipher = new KmsCredentialCipher({ keyId: 'key', region: 'us-east-1', client });
    await expect(cipher.decrypt('tenant-a', 'plain-api-key')).rejects.toThrow('not KMS encrypted');
    await expect(cipher.decrypt('tenant-a', 'kms:v1:bad*value')).rejects.toThrow('ciphertext is malformed');
    await expect(cipher.decrypt('tenant-a', 'kms:v1:')).rejects.toThrow('ciphertext is malformed');
  });
});
