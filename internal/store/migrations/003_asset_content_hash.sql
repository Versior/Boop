-- Boop migration 003: content-hash uniqueness per owner.
-- Uploads are deduplicated by SHA-256 (docs/DATABASE.md 数据完整性规则), so the
-- same bytes owned by the same user can never be stored twice even when two
-- requests race between the lookup and the insert.
-- Migration 001 is released and stays untouched.

CREATE UNIQUE INDEX idx_assets_owner_hash ON assets(owner_user_id, sha256);
