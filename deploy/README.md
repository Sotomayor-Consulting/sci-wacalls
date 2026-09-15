# Desplegar el motor con Chatwoot en otra máquina

El compose está en la **raíz del repo** (`docker-compose.yml`): levanta solo el
motor, para cuando Chatwoot ya corre en otro lugar y lo que falta es que el
motor tenga **IP pública**.

## Por qué la IP pública no es opcional

El audio va por un data channel WebRTC **directo entre el navegador del agente y
el motor**, sobre UDP. No pasa por el proxy del PaaS, ni por un túnel, ni por el
mismo puerto que la API. Dos consecuencias:

- `WACALLS_PUBLIC_IP` debe ser la IP **pública** del host. Es lo que el motor le
  anuncia al navegador como destino de los medios; con una IP privada la llamada
  conecta, suena y **no se escucha nada** — un fallo que no deja error en los
  logs.
- `WACALLS_UDP_PORT` (50000 por defecto) tiene que estar abierto en el firewall
  del proveedor, además de publicado por Docker.

El puerto HTTP (8080) sí puede ir detrás del proxy del PaaS, y **conviene que
vaya con TLS**: el navegador no carga `widget.js` por HTTP dentro de una página
HTTPS (*mixed content*), así que sin certificado el botón de llamada no aparece.

## En Dokploy

1. **Create Service → Compose** (no *Application*: los Application se
   construyen con Dockerfile o Nixpacks y no tienen ruta de compose; si se elige
   ese tipo, Dokploy trata la ruta como directorio y falla con
   `cannot create .../.env: Directory nonexistent`).
2. **Provider Git**: `https://github.com/Sotomayor-Consulting/sci-wacalls`,
   rama `main`. *Compose Path* puede quedar en su valor por defecto
   (`./docker-compose.yml`), que es donde está.
3. **Environment**: pegar las variables de [.env.example](.env.example) con los
   valores reales. Dokploy las escribe como `.env` junto al compose, y el
   compose las consume con `${VAR}`.
4. **Domains**: agregar el dominio del motor apuntando al servicio `wacalls`,
   puerto **8080**. Hace falta HTTPS: el navegador no carga `widget.js` por HTTP
   dentro de una página HTTPS. Tras agregar o cambiar un dominio hay que
   **redesplegar** para que tome efecto.
5. **Abrir el puerto UDP** (`WACALLS_UDP_PORT`, 50000 por defecto) en el
   firewall del proveedor. Traefik no enruta UDP, así que este tramo no pasa por
   el proxy.
6. **Deploy.** La imagen se construye en el servidor desde el `build: .` del
   compose — no hay registry. Es un binario Go estático (`CGO_ENABLED=0`): no
   compila opus ni ffmpeg, y por eso la grabación sale en WAV.
7. **Parear el número** por QR desde la UI del motor (pide la API key). El
   volumen es nuevo, así que la sesión de WhatsApp no viaja desde otra
   instalación.
8. **En Chatwoot**, apuntar dos cosas a este host:
   - `webhook_url` del inbox API →
     `https://ESTE-HOST/api/sessions/{SESSION_ID}/chatwoot/webhook`
   - `DASHBOARD_SCRIPTS` →
     `<script src="https://ESTE-HOST/widget.js" data-url="https://ESTE-HOST" data-api-key="WACALLS_WIDGET_KEY"></script>`

   El `{SESSION_ID}` sale de `GET /api/sessions` o del log
   `chatwoot (env): configuración aplicada`, que imprime la ruta completa.

## Con Docker a secas (sin PaaS)

El compose asume el proxy de Dokploy: el 8080 va con `expose` y el servicio se
engancha a la red externa `dokploy-network`. Sin Dokploy hay que cambiar dos
cosas: publicar el puerto HTTP (`ports: - "8080:8080"`) y quitar el bloque
`networks`, o crear esa red con `docker network create dokploy-network`.

## Configuración de la integración

Con las variables `WACALLS_CHATWOOT_*` el motor aplica la config al arrancar, a
la sesión cuyo **nombre** sea `WACALLS_CHATWOOT_SESSION`. El entorno **manda**
sobre lo guardado en el SQLite del motor: así cambiar el compose surte efecto y
no queda una config vieja ganándole en silencio.

La alternativa es `POST /api/sessions/{sid}/chatwoot`, útil para reconfigurar en
caliente o para varias sesiones. Reapuntar a otro inbox invalida el mapeo local
chat→conversación, que se recrea solo: sin eso, el motor seguiría posteando en
conversaciones del inbox anterior y Chatwoot responde `404` indefinidamente.

## Seguridad

- `WACALLS_API_KEY` es la clave **maestra** (API completa). `WACALLS_WIDGET_KEY`
  es acotada y es la única que debe ir en `DASHBOARD_SCRIPTS`, porque queda
  visible en el DOM de cualquier agente.
- Con el motor expuesto a internet, conviene además poner autenticación del
  proxy o restringir por IP el acceso al 8080.

## Si Chatwoot no está en un host público

Entonces el motor no puede postear los mensajes entrantes ni subir grabaciones,
y esa máquina se vuelve una dependencia crítica de este despliegue. Funciona
—se puede exponer Chatwoot por un túnel— pero como paso intermedio, no como
arquitectura final.
