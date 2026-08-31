delete
  from dbtx.cache
 where key = $1
   and expires_at is not null
   and expires_at <= now();
