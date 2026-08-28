-- Bob Box: add 'bob' as a valid protocol_family for custom runtime profiles.
-- This migration widens the CHECK constraint on runtime_profile.protocol_family
-- to include the Bob CLI backend added by the Bob Box fork.
ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;
ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
  CHECK (protocol_family IN (
    'claude','codebuddy','codex','copilot','opencode','deveco','openclaw',
    'hermes','pi','cursor','kimi','reasonix','dsh','kiro','antigravity',
    'qoder','qoderclicn','traecli','grok','qwen','qwenpaw','mcode','dim',
    'zeroclaw','bob'
  ));
