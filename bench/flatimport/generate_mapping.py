# Match the schema's native objects to host events without JSON conversion.
import re
from pathlib import Path
root = Path(__file__).parent
schema = (root / 'events.fbs').read_text()
aliases = {'id':'ID', 'call_id':'CallID', 'turn_id':'TurnID', 'session_id':'SessionID', 'option_id':'OptionID', 'custom_id':'CustomID', 'cost_usd':'CostUSD', 'duration_ms':'DurationMS'}
def camel(name): return ''.join(x.title() for x in name.split('_'))
out = ['package flatimport', 'import ("github.com/repogo/host/internal/agent"; "github.com/repogo/host/bench/flatimport/fb")']
for name, body in re.findall(r'table (\w+) \{([^}]+)\}', schema):
 if name == 'Events': continue
 fields = re.findall(r'(\w+):([\[\]\w]+);',body)
 for direction in ['pack','unpack']:
  src,dst = ('agent.'+name,'fb.'+name+'T') if direction=='pack' else ('fb.'+name+'T','agent.'+name)
  out += [f'func {direction}{name}(v *{src}) *{dst} {{', 'if v == nil { return nil }', f'o := &{dst}{{}}']
  for field,typ in fields:
   a,g = aliases.get(field,camel(field)),camel(field)
   s,d = (a,g) if direction=='pack' else (g,a)
   if typ.startswith('[') and typ != '[ubyte]':
    inner=typ[1:-1]
    dt='*fb.'+inner+'T' if direction=='pack' else 'agent.'+inner
    arg='&x' if direction=='pack' else 'x'
    deref='' if direction=='pack' else '*'
    out += [f'if v.{s} != nil {{ o.{d} = make([]{dt}, 0, len(v.{s})); for _, x := range v.{s} {{ o.{d} = append(o.{d}, {deref}{direction}{inner}({arg})) }} }}']
   elif typ[0].isupper(): out += [f'o.{d} = {direction}{typ}(v.{s})']
   else:
    expr=f'v.{s}'
    if name=='Event' and field=='kind': expr=('string' if direction=='pack' else 'agent.EventKind')+'('+expr+')'
    out += [f'o.{d} = {expr}']
  out += ['return o','}']
(root/'mapping.go').write_text('\n'.join(out)+'\n')
