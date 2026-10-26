INSERT INTO users (name, email) VALUES ('Jack', 'jackwillis517@gmail.com')
ON CONFLICT (email) DO NOTHING;
