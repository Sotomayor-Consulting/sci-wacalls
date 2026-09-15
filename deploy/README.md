# Desplegar el motor con Chatwoot en otra máquina

Este compose levanta **solo** el motor. Sirve cuando Chatwoot ya corre en otro
lugar y lo que hace falta es que el motor tenga **IP pública**.

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

## Pasos

1. **Construir la imagen** (no se publica en ningún registry):
   ```bash
   git clone -b feat/chatwoot-integration https://github.com/Sotomayor-Consulting/sci-wacalls
   cd sci-wacalls && docker build -t sci-wacalls:dev .
   ```
   Es un binario Go estático (`CGO_ENABLED=0`): no compila opus ni ffmpeg, y la
   grabación sale en WAV por eso mismo.
2. **Configurar** `.env` a partir de [.env.example](.env.example). `WACALLS_CHATWOOT_URL`
   va con la URL **pública** de Chatwoot, no `http://rails:3000`.
3. **Levantar**:
   ```bash
   docker compose -f docker-compose.vps.yaml up -d
   ```
4. **Parear el número** por QR desde la UI del motor. El volumen es nuevo, así
   que la sesión de WhatsApp no viaja desde otra instalación.
5. **En Chatwoot**, apuntar dos cosas a este host:
   - `webhook_url` del inbox API →
     `https://ESTE-HOST/api/sessions/{SESSION_ID}/chatwoot/webhook`
   - `DASHBOARD_SCRIPTS` →
     `<script src="https://ESTE-HOST/widget.js" data-url="https://ESTE-HOST" data-api-key="WACALLS_WIDGET_KEY"></script>`

   El `{SESSION_ID}` sale de `GET /api/sessions` o del log
   `chatwoot (env): configuración aplicada`, que imprime la ruta completa.

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
