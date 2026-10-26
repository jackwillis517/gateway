INSERT INTO models (
    name, alias, provider, upstream_model,
    input_price_per_mtok, output_price_per_mtok,
    cache_read_price_per_mtok, cache_write_price_per_mtok,
    max_output_tokens
)
VALUES
(
    'ollama/qwen3.5:2b-32k', 'qwen-local', 'ollama', 'qwen3.5:2b-32k',
    0, 0, 0, 0, 32768
)
ON CONFLICT (name) DO NOTHING;
