-- Client-DECLARED media metadata: filename, alt text, pixel dimensions.
--
-- The dividing line for every media column is "who can possibly know". size_bytes,
-- content_type and uploaded_at are facts the platform OBSERVED against the bucket,
-- which is why MarkMediaUploaded takes them from a Stat and its comment says
-- "never from the client". These four cannot be observed at all: the storage key
-- is random and carries no original name, and the bytes never pass through the
-- API. objectstore.Store exposes exactly PresignPost / PresignGet / Stat / Delete,
-- and Stat answers {Size, ContentType} — there is no read path to measure a pixel
-- with. Adding one means pulling the object back through the API, which ADR-005
-- rules out as the whole point of the presigned design; and avif is on the upload
-- whitelist while Go has no decoder for it, so even that work would still leave
-- NULLs here.
--
-- So these columns hold a CLIENT CLAIM, unverified, and this is the place that
-- says so out loud rather than letting a later reader assume the platform checked.
-- The blast radius is bounded, and stating it is what makes the trade acceptable:
-- a lying client makes ITS OWN site's layout jump. It cannot cross a tenant
-- boundary (RLS decides that), cannot inflate a quota (size_bytes is
-- server-observed), and cannot make a byte readable (AssetIsPublished decides
-- that). Nothing downstream may treat these as authenticated facts.
--
-- All four are NULLABLE with no backfill, for the reason 000021 gives for the
-- authorship columns: existing rows genuinely have no declared name or
-- dimensions, and a manufactured value in a descriptive column is worse than an
-- honest absence.
--
-- No index. Nothing queries by filename — media is reached by id, from an entry
-- payload — and an index without a query is speculation (ADR-005 records the
-- trigger: a tenant-facing media library with search).

ALTER TABLE media_assets
    ADD COLUMN IF NOT EXISTS filename  TEXT,
    ADD COLUMN IF NOT EXISTS alt_text  TEXT,
    ADD COLUMN IF NOT EXISTS width_px  INTEGER,
    ADD COLUMN IF NOT EXISTS height_px INTEGER;

-- alt_text is a TRI-STATE and is deliberately NOT `NOT NULL DEFAULT ''`.
--
--   NULL  — nobody has described this image yet.
--   ''    — an editor looked at it and said it IS decorative.
--
-- Those are different answers and only one of them licenses `alt=""` in the
-- rendered markup. A renderer that cannot tell them apart emits `alt=""` for the
-- undescribed case, which asserts "this image carries no information" on the
-- editor's behalf — a claim nobody made, invisible to every reader who is not
-- using a screen reader, and therefore never reported as a bug. A DEFAULT ''
-- would make every row that predates this migration make that claim on day one,
-- which is precisely why the cheaper NOT NULL shape is refused.
--
-- The 1000-character ceiling is a backstop against a caller pasting an article
-- into the field, not an editorial rule: alt text is read aloud in one breath.
ALTER TABLE media_assets
    DROP CONSTRAINT IF EXISTS media_assets_alt_text_check;
ALTER TABLE media_assets
    ADD CONSTRAINT media_assets_alt_text_check
    CHECK (alt_text IS NULL OR char_length(alt_text) <= 1000);

-- filename is echoed straight back to admin UIs and is the obvious future source
-- for a Content-Disposition header, so it is constrained in the DATABASE rather
-- than trusted to whatever write path happens to exist. Two failure modes are
-- being closed, both of which are silent until they are not:
--
--   * a path separator makes a declared name read as a PATH. Today that is only
--     a misleading label; the moment anything joins it to a directory or a URL it
--     is traversal, and the value was already stored by then.
--   * a control character — CR or LF above all — is header injection on the day
--     this string reaches a response header, which is the single most likely
--     next use for it.
--
-- Empty is NOT "no filename"; NULL is. Accepting '' would give the system two
-- spellings of the same absence, and the one that is not NULL is the one a UI
-- renders as a blank name instead of falling back to "untitled".
ALTER TABLE media_assets
    DROP CONSTRAINT IF EXISTS media_assets_filename_check;
ALTER TABLE media_assets
    ADD CONSTRAINT media_assets_filename_check
    CHECK (
        filename IS NULL
        OR (char_length(filename) BETWEEN 1 AND 255
            AND filename !~ '[/\\]'
            AND filename !~ '[[:cntrl:]]')
    );

-- Both dimensions or neither, and each within 1..65535.
--
-- The ONLY reason these two columns exist is to let a renderer reserve layout
-- space before the image loads. That needs both numbers — one alone reserves
-- nothing, so a half-filled pair buys exactly zero of the benefit the columns
-- were added for. It is also worse than absent: a front end handed a width and
-- no height does not degrade to "unknown", it computes an aspect ratio from the
-- half it has and draws a confidently wrong box. The biconditional is what makes
-- "present" mean "usable" for every reader, forever, without each of them
-- re-deriving the rule.
--
-- 0 is not a picture. 65535 is far past any real asset while leaving the pair
-- comfortably inside INTEGER, so the ceiling costs nothing and stops a declared
-- dimension from being an arithmetic hazard downstream.
ALTER TABLE media_assets
    DROP CONSTRAINT IF EXISTS media_assets_dimensions_check;
ALTER TABLE media_assets
    ADD CONSTRAINT media_assets_dimensions_check
    CHECK (
        (width_px IS NULL) = (height_px IS NULL)
        AND (width_px IS NULL
             OR (width_px  BETWEEN 1 AND 65535
                 AND height_px BETWEEN 1 AND 65535))
    );
