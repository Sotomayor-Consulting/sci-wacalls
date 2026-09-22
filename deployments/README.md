# Desplegar el motor

Dos variantes, una por PaaS. La diferencia entre ambas no es cosmética: cada
una resuelve distinto **a qué puerto interno enruta el proxy** y **cómo se ven
entre sí dos recursos del mismo servidor** — mezclarlas produce un 404 o un
timeout, según cuál regla se aplique donde no corresponde.

| | [`dokploy/`](dokploy) | [`coolify/`](coolify) |
|---|---|---|
| Red entre recursos | `dokploy-network` declarada en el compose (compartida) | Aislada por stack — **no** declarar `networks:` propia; se conecta desde la UI |
| Cómo se ven dos recursos | nombre corto del servicio (`rails`) | igual, pero primero activar *Connect to Predefined Network* en los dos |
| `expose: 8080` | obligatorio | obligatorio |
| Puerto de medios (UDP) | publicado de verdad, no vía proxy | publicado de verdad, no vía proxy |

## Lo que nunca cambia, sea cual sea la plataforma

- **El dominio público es para el navegador, no para contenedores.** Un
  contenedor que le habla a otro en el mismo servidor por su dominio público
  puede fallar por *hairpin NAT* — el paquete sale a internet y no vuelve. Eso
  ya pasó una vez. Server-a-servidor siempre por red interna.
- **`WACALLS_CHATWOOT_URL` va con `http://`, nunca `https://`** cuando apunta a
  una dirección interna — el motor no termina TLS, y pedirle `https://` da
  *wrong version number*.
- La clave que va en `DASHBOARD_SCRIPTS` de Chatwoot es la **acotada**
  (`WACALLS_WIDGET_KEY`), nunca la maestra — queda visible en el DOM de
  cualquier agente.
- Sin `expose: 8080`, el síntoma es 404 en **todas** las rutas del dominio,
  incluidas las que sí existen — el proxy nunca llega a preguntarle nada al
  proceso, no es un bug del motor.
