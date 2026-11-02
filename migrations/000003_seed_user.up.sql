INSERT INTO users (name, email, password_hash)
VALUES ('Jack', 'jackwillis517@gmail.com', '$2a$12$7roYpdPHjjd/J3QY9tjPw./EhFbUnA4Z6rog.VuiMfJcS6uv1s1Fm')
ON CONFLICT (email) DO NOTHING;
