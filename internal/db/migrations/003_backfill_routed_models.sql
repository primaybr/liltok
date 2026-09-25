-- Rows logged before requested_model existed recorded the client's model as the served model for
-- routed Gemini and Groq replies. Move it to requested_model and record the model that served them.
UPDATE request_logs
SET requested_model = model, model = 'gemini-3.8-flash'
WHERE provider = 'gemini'
  AND (requested_model IS NULL OR requested_model = '' OR requested_model = model)
  AND model LIKE 'claude%';

UPDATE request_logs
SET requested_model = model, model = 'qwen/qwen3.8-27b'
WHERE provider = 'groq'
  AND (requested_model IS NULL OR requested_model = '' OR requested_model = model)
  AND (model LIKE 'claude%' OR model = 'free-first');
