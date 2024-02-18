# distributed-calculator

Команды для запуска и настройки postgres и rabbitmq:
```
docker pull postgres
docker run --name pg-container -e POSTGRES_PASSWORD=pass -d -p 5432:5432 postgres
docker run -d --hostname my-rabbit --name rbmq-container -p 15672:15672 -p 5672:5672 rabbitmq:3-management
```

Создание таблиц в бд:
```
docker exec -ti pg-container psql -U postgres
```
После введения этой команды вы попадаете в psql, там нужно испонить следующие команды:
```
CREATE DATABASE "go-pg";
\c go-pg
CREATE TABLE calculations (id SERIAL PRIMARY KEY, expression TEXT, status TEXT, answer DOUBLE PRECISION);
CREATE TABLE agents (id SERIAL PRIMARY KEY, last_seen TEXT, status TEXT, goroutines INT);
CREATE TABLE settings (name TEXT, value INT);
INSERT INTO settings VALUES ('add', 5), ('sub', 5), ('mult', 5), ('div', 5), ('del', 60);
```
