-- name: CompareAndDelete :exec
delete
  from dbtx.cache
 where key = @ key
   and (digest = @ digest or (expires_at is not null and expires_at <= now()));
