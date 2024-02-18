# distributed-calculator
`distributed-calculator` - это финальный проект второго спринта курса Go в Яндекс Лицее. Он представляет собой сервер и агентов, которые могут обрабатывать и решать выражения, разбивая их на подвыражения и решая максимально возможное количество операций параллельно для достижения большей эффективности (исходя из условий, описанных в ТЗ). В разработке использовались такие технологии, как `docker`, `postgres`, `rabbitmq`, а фронтенд был написан с помощью `htmx` для приятного взаимодействия пользователя с проектом. Ниже приведены шаги по установке и запуску всех необходимых ресурсов для запуска проекта, а также теоретическая часть, объясняющая, как все работает и взаимодействует друг с другом.

## Установка проекта

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

В папке http:
```
go run server.go
```
в папке проекта:
```
go run agent.go
```
