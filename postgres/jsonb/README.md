# dbtx/postgres/jsonb

## Joining Multiple Tables

```sql
SELECT 
    -- 1. Aggregates all joined rows into a single JSON array
    jsonb_agg(
        -- 2. Constructs the JSON object with table names as keys
        jsonb_build_object(
            'user', to_jsonb(u),
            'order', to_jsonb(o)
        )
    ) AS joined_data
FROM users u
LEFT JOIN orders o ON u.id = o.user_id;
```
