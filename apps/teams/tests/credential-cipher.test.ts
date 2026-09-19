import { describe, expect, it } from 'vitest';
import type { KMSClient } from '@aws-sdk/client-kms';
import { KmsCredentialCipher } from '../src/credential-cipher.js';

describe('KMS credential cipher', () => {
  it('round-trips credentials and binds both operations to the tenant context', async () => {
    const requests: Array<Record<string, unknown>> = [];
    const client = {
      send: async (command: { readonly input: Record<string, unknown> }) => {
        requests.push(command.input);
        if ('Plaintext' in command.input) throw new Error('shared KMS endpoint denies Encrypt');
        if ('KeySpec' in command.input) return { Plaintext: Buffer.alloc(32, 7), CiphertextBlob: Buffer.from('wrapped-test-key') };
        return { Plaintext: Buffer.alloc(32, 7) };
      },
    } as unknown as KMSClient;
    const cipher = new KmsCredentialCipher({ keyId: 'arn:aws:kms:us-east-1:123:key/test', region: 'us-east-1', client });

    const encrypted = await cipher.encrypt('tenant-a', 'secret-api-key');
    await expect(cipher.decrypt('tenant-a', encrypted)).resolves.toBe('secret-api-key');
    expect(encrypted.startsWith('kms:v2:')).toBe(true);
    expect(encrypted).not.toContain('secret-api-key');
    expect(requests).toHaveLength(2);
    expect(requests[0]?.KeyId).toBe('arn:aws:kms:us-east-1:123:key/test');
    expect(requests[0]?.KeySpec).toBe('AES_256');
    expect(requests[0]).not.toHaveProperty('Plaintext');
    expect(requests[0]?.EncryptionContext).toEqual({ domain: 'qurl-teams-tenant-credential', tenant_id: 'tenant-a', field: 'qurl_api_key' });
    expect(requests[1]?.EncryptionContext).toEqual(requests[0]?.EncryptionContext);
    await expect(cipher.decrypt('tenant-b', encrypted)).rejects.toThrow();
    const parts = encrypted.split('.');
    const payload = Buffer.from(parts.at(-1) ?? '', 'base64url');
    payload[0] = (payload[0] ?? 0) ^ 1;
    parts[parts.length - 1] = payload.toString('base64url');
    await expect(cipher.decrypt('tenant-a', parts.join('.'))).rejects.toThrow();
  });

  it('decrypts the stored v2 format independently of current encryption code', async () => {
    const client = { send: async () => ({ Plaintext: Buffer.alloc(32, 7) }) } as unknown as KMSClient;
    const cipher = new KmsCredentialCipher({ keyId: 'key', region: 'us-east-1', client });
    const stored = 'kms:v2:d3JhcHBlZC10ZXN0LWtleQ.AwMDAwMDAwMDAwMD.GaViTQlTam8Zs8PlzEsy9Q.VpvAcT9ccyMKKW43ji4';
    await expect(cipher.decrypt('tenant-a', stored)).resolves.toBe('secret-api-key');
    await expect(cipher.decrypt('tenant-b', stored)).rejects.toThrow();
  });

  it('rejects plaintext or malformed stored credentials', async () => {
    const client = { send: async () => ({ Plaintext: new Uint8Array([1]) }) } as unknown as KMSClient;
    const cipher = new KmsCredentialCipher({ keyId: 'key', region: 'us-east-1', client });
    await expect(cipher.decrypt('tenant-a', 'plain-api-key')).rejects.toThrow('not KMS encrypted');
    await expect(cipher.decrypt('tenant-a', 'kms:v2:bad*value')).rejects.toThrow('ciphertext is malformed');
    await expect(cipher.decrypt('tenant-a', 'kms:v2:')).rejects.toThrow('ciphertext is malformed');
  });
});
