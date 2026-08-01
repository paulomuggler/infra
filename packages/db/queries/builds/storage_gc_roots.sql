-- name: GetStorageGCRoots :many
-- Every build the registry considers live, for template-storage GC.
--
-- Storage GC keeps a build directory only if it is reachable, through header
-- block mappings, from one of these. Anything left out is deleted, so each
-- clause below is a safety property rather than an optimisation.
SELECT DISTINCT r.build_id::uuid AS build_id
FROM (
    -- 1. What a spawn resolves to, for every template and every tag. This
    -- mirrors GetTemplateWithBuildByTag exactly: newest 'ready' assignment per
    -- (env, tag). It covers source='template', 'snapshot' (a paused sandbox's
    -- snapshot env) and 'snapshot_template' uniformly.
    SELECT latest.build_id
    FROM (
        SELECT DISTINCT ON (eba.env_id, eba.tag) eba.build_id
        FROM public.env_build_assignments eba
        JOIN public.env_builds eb ON eb.id = eba.build_id AND eb.status_group = 'ready'
        ORDER BY eba.env_id, eba.tag, eba.created_at DESC
    ) latest

    UNION

    -- 2. Every assignment of an env that backs a live paused sandbox, not just
    -- the newest. A paused sandbox IS its RAM image: losing it loses the
    -- sandbox, and pause suspends the TTL clock so it can sit there forever.
    -- UpsertSnapshot mints the snapshots row and this assignment edge in one
    -- statement, so this clause catches every live paused sandbox; killing one
    -- deletes its snapshot env, which is what makes the orphaned build rows it
    -- leaves behind collectable.
    SELECT eba.build_id
    FROM public.snapshots s
    JOIN public.env_build_assignments eba ON eba.env_id = s.env_id

    UNION

    -- 3. Persistent snapshot templates.
    SELECT st.build_id
    FROM public.snapshot_templates st
    WHERE st.build_id IS NOT NULL

    UNION

    -- 4. In-flight builds, which nothing else references yet.
    SELECT eb.id
    FROM public.env_builds eb
    WHERE eb.status_group IN ('pending', 'in_progress')

    UNION

    SELECT atb.build_id
    FROM public.active_template_builds atb
) r;
