# SDK 2.x dependency

This change targets @layervai/qurl 2.0.0 and keeps NHP at 1.1. Publish the SDK
and its optional native state package before merging this dependency update.
The npm registry currently has SDK 0.7.0. The new package lock entries have no
fabricated integrity hashes; refresh them from the registry after publication.

The migration was tested against the local SDK 2.0.0 build. To reproduce before
publication, build the SDK and install it into this app without changing its
release dependency:

    npm install --no-save --package-lock=false /absolute/path/to/qurl-typescript
    npm run lint
    npm test -- --runInBand

Delegated batch create and poll requests now use QURLClient. Discord retains its
interaction deadline, retry budget, delivery mapping, and cleanup behavior.
A lost POST response is retried only with the same idempotency key and body.
Resource status and revoke tests now use CRIDs and check that legacy resource
IDs fail before network access, as required by SDK 2.x.
