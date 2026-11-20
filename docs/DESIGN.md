# cardo — design notes

# Cardo — diseño del tool público de swap de motores

**Nombre elegido**: `cardo` (latín, *la bisagra*: la pieza mecánica que decide hacia dónde se abre
algo). Se eligió sobre nombres de estrellas porque es una herramienta pequeña, no un proyecto, y
porque la palabra no está saturada en el ecosistema Go.

- Repo: `github.com/JoyanLabs/cardo` (público, creado el 2026-11-19, `main` protegida con PR + 1 aprobación)
- Binario: `cardo`
- Invocación: `cardo up vllm-qwen`, `cardo watch`, `cardo status`
- Licencia: MIT
- Marca: `assets/cardo.svg` (eje con dos goznes y el polo arriba)
- Referencia clásica real: *cardo caeli* = el polo celeste

### Regla del README

El README **no explica el origen del nombre**. Nada de "cardo significa bisagra" ni etimologías. Va
la marca, una línea de qué hace, instalación y uso. El nombre se sostiene solo.

### La referencia clásica (para el logo, no para el README)

`cardo caeli` no es invento: está atestiguado en latín clásico con el sentido astronómico de **polo**.

- **Varrón**, *De re rustica* 1.2.4: *"inter circulum septemtrionalem et inter **cardinem caeli**"*.
- **Lewis & Short** glosan *cardo*: "the point about which something turns, a pole", citando
  justamente Varrón y, para el polo norte, *"caeli"*.
- **Cicerón**, *De natura deorum* 2.41: *"duplici de cardine vertex"* — el polo apoyado en un
  **gozne doble**, arriba y abajo. De ahí sale la marca.
- **Séneca**, *Hercules furens* 1139: *"cardo glacialis ursae"* — el gozne de la Osa.

**Lo que NO se puede afirmar** (verificado como falso o anacrónico):
- "Draco es la serpiente que envuelve el eje" es descripción moderna, no epíteto antiguo.
- En época romana el polo **no tenía estrella visible**: no se puede dibujar "la estrella Cardo".
- `cardines caeli` como fórmula de cuatro puntos no está atestiguado; el plural clásico es
  `cardines orbis terrarum`.

### Los tres conceptos de marca, evaluados

| Concepto | Honestidad de la referencia | Legible a 32 px |
|---|---|---|
| **Eje con dos goznes** (elegido) | alta: es literalmente Cicerón | **sí**, sobrevive a 16 px |
| Cruz cardo × decumanus con el polo al centro | media: la parte celeste es decorativa | sí |
| Cynosura: siete puntos de la Osa Menor y un arco | alta (Arato) | **no**: los puntos desaparecen |
 El objetivo es un binario genérico que cambie qué
modelo ocupa una GPU, con lo que aprendimos y con lo que le falta a `llama-swap`.

## Qué problema resuelve

Una máquina con una GPU y varios modelos. Querés servir uno por vez, avisar antes de bajar el que
está en uso, y que si el nuevo no arranca vuelva el anterior. Hoy eso es un script de 102 líneas o
una herramienta que corta los requests en vuelo.

**Dominio**: no es "swappear GPUs". Es **un slot por GPU** y un registro de motores que compiten por
ese slot. El vocabulario del código es *engine* y *slot*, nunca "gpu swap".

## Modelo de datos

Un archivo por motor, formato plano clave=valor (el mismo de hoy, que ya funciona):

```conf
NAME="vllm-qwen"
DESCRIPTION="Qwen3.8-27B NVFP4 en vLLM"
TYPE="docker"                  # docker | compose | command
CONTAINER="vllm-qwen"
HEALTH="http://localhost:8000/v1/models"
BUSY="http://localhost:8000/metrics"     # cómo se pregunta si hay trabajo
DRAIN="1"
WAIT="240"
SLOT="0"                       # qué GPU ocupa
```

## Verbos

| Verbo | Qué hace |
|---|---|
| `list` | motores registrados, cuál está activo, salud |
| `status` | estado vivo con memoria real medida |
| `up <name>` | drena el actual, lo para, levanta este, verifica, rollback si falla |
| `down` | baja el activo (con drenado) |
| `watch` | TUI: qué está residente, memoria, peticiones en vuelo (absorbe `comfy-mem`) |
| `doctor` | valida los `.conf` y la conectividad de cada health |

`watch` y el muestreo de memoria son el mismo dominio: "qué está pasando ahora en este slot". Por eso
van en el mismo binario y no en dos herramientas chicas.

## Lo que lo diferencia de lo que existe

| Idea | Origen |
|---|---|
| **Drenado por tipo de motor**: la cola de ComfyUI no es un request HTTP en vuelo | spark-switchboard |
| **Health que reinicia el timeout con progreso de carga** (no abortar a los N segundos si avanza) | llama-swap |
| **Rollback al motor anterior** si el nuevo no responde | nuestro (no lo tiene nadie) |
| **No tocar lo que no arrancó**: si algo corre fuera del registro, se avisa y no se para | DGX-Model-Manager |
| Cooldown tras arranque fallido, para no reintentar en bucle | spark-switchboard |
| Registrar cuándo se verificó por última vez cada chequeo | model-mirror |

## Higiene para publicar

- Cero rutas absolutas del laboratorio en el código.
- Cero nombres de personas, organizaciones o modelos en la lógica. Los `.conf` de ejemplo pueden
  nombrar motores reales: son datos, no código.
- Un solo binario, `go install` y releases con `linux/{amd64,arm64}`.
- Tests con un motor falso (un servidor HTTP de prueba) para drenado, timeout y rollback.
- Licencia MIT o Apache-2.0.

## Estado

Skeleton merged (mark, README, community files, `examples/` case study). The CLI is next; this
document keeps the implementation from re-deciding and gives reviewers a baseline.
