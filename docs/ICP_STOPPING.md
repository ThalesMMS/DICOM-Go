# ICP: objetivo e motivo de parada


## Objetivo monitorado e transform devolvido

Para o conjunto fixo amostrado `S`, a distância é
`d(T,p) = min(q∈moving) |T(p)-q|`. O ICP reporta:

- `MeanDistance = sum(d(T,p))/len(S)`, a média absoluta em mm;
- `RMS = sqrt(sum(d(T,p)^2)/len(S))`, em mm.

Ambos são avaliados no **Transform devolvido**, inclusive ao esgotar o limite
de uma única iteração. As novas correspondências dessa avaliação alimentam o
próximo refit. `Tolerance` continua um limiar **absoluto de melhora da média**,
em mm, com default `1e-4` quando zero. Não virou limiar RMS, tolerância relativa
ou limite de erro residual. O refit least squares minimiza erro quadrático dos
pares: a média absoluta não precisa cair junto com RMS.

| `StopReason` | Condição | `Converged` | `Iterations` |
|---|---|---|---|
| `ICPStopInitiallyAligned` | Média inicial exatamente zero e clone matricial admissível ao Mode também mede média/RMS zero | true | 0 |
| `ICPStopTolerance` | Após o refit aplicado, `0 <= previousMean-newMean < Tolerance` | true | Número de refits aplicados |
| `ICPStopWorsened` | Após o refit aplicado, `previousMean-newMean < 0` | false | Inclui esse refit |
| `ICPStopMaximumIterations` | Budget esgotado sem os critérios acima | false | Número de refits aplicados |

`ICPStopTolerance` inclui **estabilidade da média** (`delta=0`), mesmo quando a
média residual é grande. Essa flag não prova pequeno residual, mínimo geométrico
local/global ou precisão de registro. Uma piora não é mascarada por um limiar
de melhora positivo. O refit que piora continua no resultado, com distâncias e
motivo correspondentes; não há rollback implícito.

A saída inicial exata só ocorre depois de validar entrada e degenerescência
pelo fitter. Ela conserva os coeficientes do Initial efetivamente utilizado,
por clone, sem aplicar o refit; a matriz deve pertencer à família solicitada
usando os verificadores existentes. Initial não matricial, clone incompatível
ou matriz fora da família continuam pelo refit normal. Sem Initial explícito,
o deslocamento de centroides já usado pelo algoritmo também pode ser exato.

“Exatamente zero” é o valor calculado em float64, sem margem numérica nova.
Uma melhora negativa minúscula de uma fonte inicialmente não exata continua
reportada como `worsened`; a magnitude e RMS ajudam o caller a avaliá-la. A
saída inicial evita que uma transformação já exata adquira ruído somente por
um refit desnecessário. Não se presume que as distâncias tenham precisão
arbitrária em qualquer escala de coordenadas.

## Fixture analítica independente

Fixed contém o centro e os seis pontos `±100 mm` em X, Y e Z. Moving contém os
mesmos seis pontos e o centro deslocado para `(4,0,0)`. A configuração determina
um fit 3D não degenerado. As correspondências são inequívocas perto da
identidade; os outros vértices ficam a aproximadamente 100 mm do centro.

A covariância cruzada continua diagonal/isotrópica, pois o único ponto alterado
é o centro fixo. Rigid, similarity e affine dão `M=I`, `offset=(4/7,0,0)`. As
métricas e o deslocamento decorrem diretamente dessa geometria; o teste calcula
nearest points por brute force independente do KD-tree/residuals do fitter.

| Métrica | Initial identidade | Refit analítico |
|---|---:|---:|
| Média absoluta | `4/7 = 0,5714285714 mm` | `48/49 = 0,9795918367 mm` |
| RMS de distância | `4/sqrt(7) = 1,5118578920 mm` | `4*sqrt(6)/7 = 1,3997084244 mm` |
| RMS de deslocamento aplicado | 0 | `4/7 mm` |

No baseline `b5220e80b1b4ab5430a4aaed0ea5118adfd45bc8`, `previous-mean <
Tolerance` marcou `Converged=true` na segunda iteração, porque a diferença da
média era negativa. A média/RMS finais já eram recalculadas corretamente; o
defeito confirmado está na decisão de parada com valores anteriores ao refit,
não nesses valores finais. Agora a primeira iteração retorna esse mesmo refit,
`Converged=false` e `ICPStopWorsened`. A queda de RMS não é apresentada como
queda do objetivo de média.

A mesma solução LS utilizada como Initial tem média residual aproximadamente
0,98 mm e `delta=0`: retorna `ICPStopTolerance`, ilustrando explicitamente a
estabilidade sem pequeno erro residual.

## Compatibilidade, limites e validação

A assinatura e os campos de `ICPOptions` permanecem. `StopReason` é campo
adicional de `ICPResult`; código que construía esse resultado com literal
posicional precisa migrar para literal nomeado. Quem assumia `Iterations>=1`
deve aceitar zero para `initially-aligned`. Essa saída devolve o clone do Initial
matricial: seu tipo concreto pode ser o do Initial, mesmo quando ele representa
a família solicitada. Prefira `MatrixTransform` às assertions de tipo concreto.
A flag `Converged` deixou de aceitar piora; callers devem examinar o motivo e as
distâncias antes de armazenar um Spatial Registration.

`MaxIterations` mantém default 50 e `MaxFixedPoints` default 20000; o sample
continua `fixed[i*len(fixed)/MaxFixedPoints]`, determinado uma vez por chamada.
As métricas são desse sample, não necessariamente de toda a nuvem fixed.
Moving continua inteiro no KD-tree: `MaxFixedPoints` não é limite de RSS ou do
número de pontos moving. As entradas são emprestadas/read-only, e Initial não
pode ser mutado em paralelo pelo caller.

Tolerância negativa/NaN/Inf, pontos não finitos e degenerescência conservam
categorias explícitas de erro. Distâncias ou somas não representáveis retornam
`ErrLandmarks` em vez de acessar um nearest inexistente. Cancelamento é checado
na validação, por chunks de até 256 pontos nas métricas, antes de refits e ao
terminar uma avaliação; sorting do KD-tree e o fitter síncrono não são
preemptados no meio de uma chamada. Erros/cancel devolvem resultado zero, sem
transform parcial. Nenhum worker/goroutine novo foi criado.

As regressões cobrem a fixture analítica nos três modos, métricas no refit,
limite de uma iteração, plateau com residual nãozero, Initial exato com
rotação/offset e clone independente, família incompatível, phantom transform
conhecido, observação parcial/outliers, sample/limits, degenerescência,
cancelamento e determinismo. Não se acrescentou trimming/robust loss,
multi-start, novo solver ou critério relativo. Fixtures e testes locais não
são qualificação clínica, de performance ou de PACS de destino.

Validação focada:

```sh
go test ./registration -run 'TestICP|TestIterativeClosestPoint|TestRegisterLandmarks|TestKDTree' -count=1
go test -race ./registration -run 'TestICP|TestIterativeClosestPoint|TestRegisterLandmarks|TestKDTree' -count=1
```
