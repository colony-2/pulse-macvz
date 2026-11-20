import importlib.util
import json
import os
import shutil
from pathlib import Path
import subprocess
import sys
from tempfile import TemporaryDirectory
import unittest

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('names', ROOT/'scripts/image-names.py')
names = importlib.util.module_from_spec(spec)
spec.loader.exec_module(names)

FAKE = r'''import copy,fcntl,hashlib,json,os,shutil,sys,time
from pathlib import Path
state=Path(os.environ['FAKE_TART_STATE'])
args=sys.argv[1:]
tool=Path(sys.argv[0]).name
with open(str(state)+'.lock','w') as lock:
 fcntl.flock(lock,fcntl.LOCK_EX)
 d=json.loads(state.read_text());vms=d['vms'];d['calls'].append([tool,*args]);waiting=None;result='';code=0
 if tool=='tart':
  cmd=args[0]
  if cmd=='clone' and '--help' in args: result='--overwrite'
  elif cmd=='get':
   if args[1] not in vms: code=1
   else: result=json.dumps(vms[args[1]])
  elif cmd=='list': result=json.dumps(list(vms.values()))
  elif cmd=='clone':
   rest=[a for a in args[1:] if a!='--overwrite'];src,dst=rest
   if '--overwrite' in args and os.environ.get('FAIL_PUBLISH')==dst:
    if os.environ.get('REMOVE_FAILED_DESTINATION'): vms.pop(dst,None)
    result='simulated publication failure';code=1
   elif dst in vms and vms[dst]['State']=='running': code=1
   else:
    d['next']+=1
    vm=copy.deepcopy(vms.get(src,{'OS':'darwin','CPU':2,'Memory':4096,'State':'stopped','MACAddress':'raw','Release':'raw'}))
    vm.update(Name=dst,IP='192.0.2.'+str(d['next']));vms[dst]=vm
    if '--overwrite' in args and os.environ.get('FAIL_AFTER_PUBLISH') in (dst,'*'):
     result='simulated native error after replacing the destination';code=1
     if os.environ.get('CORRUPT_AFTER_PUBLISH'): vm['Release']='corrupt'
  elif cmd=='set':
   vm=vms[args[1]];vm.update(CPU=int(args[args.index('--cpu')+1]),Memory=int(args[args.index('--memory')+1]),MACAddress='fresh-'+vm['IP'])
   if '--random-serial' in args: vm['MachineID']='machine-'+vm['IP']
  elif cmd=='run':
   waiting=args[-1];vms[waiting]['State']='running'
  elif cmd=='ip': result=vms[args[1]]['IP']
  elif cmd=='suspend': vms[args[1]]['State']='suspended'
  elif cmd=='stop': vms[args[1]]['State']='stopped'
  elif cmd=='delete': vms.pop(args[1],None)
  else: code=1
 elif tool=='ssh':
  address=next(a.split('@')[1] for a in args if '@192.0.2.' in a)
  vm=next(v for v in vms.values() if v['IP']==address)
  command=args[-1]
  if command=='true': pass
  elif command.startswith('mktemp'): result='/tmp/pulse-image-setup.TEST'
  elif 'prepare-base.sh' in command:
   vm.update(State='stopped',Release='new');result='PULSE_BASE_READY'
  elif 'prepare-template.sh' in command:
   if os.environ.get('FAIL_SLOT') and vm['Name'].endswith('-slot-'+os.environ['FAIL_SLOT']): code=1
   else: result='PULSE_TEMPLATE_READY {}'
   if os.environ.get('DELETE_ON_PREPARE'): vms.pop(os.environ['DELETE_ON_PREPARE'],None)
  else: code=1
 elif tool=='scp': pass
 else: code=1
 if os.environ.get('FAKE_VM_FILES'):
  def put(path,data):
   if not path.exists() or path.read_text()!=data: path.write_text(data)
  directory=Path(os.environ['TART_HOME'])/'vms';directory.mkdir(parents=True,exist_ok=True)
  for child in directory.iterdir():
   if child.name not in vms: shutil.rmtree(child)
  for name,vm in vms.items():
   if not all(k in vm for k in ('MACAddress','CPU','Memory','Release')): continue
   path=directory/name;path.mkdir(exist_ok=True)
   digest=hashlib.sha256(vm['MACAddress'].encode()).hexdigest()[:10]
   mac='02:'+':'.join(digest[i:i+2] for i in range(0,10,2))
   config=dict(cpuCount=vm['CPU'],memorySize=vm['Memory'],macAddress=mac,ecid=vm.get('MachineID','original'))
   put(path/'config.json',json.dumps(config))
   put(path/'disk.img','disk:'+vm['Release'])
   put(path/'nvram.bin','nvram:'+vm.get('MachineID','original'))
   saved=path/'state.vzvmsave'
   if vm['State']=='suspended': put(saved,'saved:'+vm.get('MachineID','original'))
   elif saved.exists(): saved.unlink()
 state.write_text(json.dumps(d))
print(result)
if waiting:
 for _ in range(3000):
  with open(str(state)+'.lock','w') as lock:
   fcntl.flock(lock,fcntl.LOCK_EX)
   vm=json.loads(state.read_text())['vms'].get(waiting)
  if not vm or vm['State']!='running': break
  time.sleep(.01)
 else: sys.exit(2)
sys.exit(code)
'''

class UpdateImages(unittest.TestCase):
    def setUp(self):
        self.temp=TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name).resolve();binary=self.root/'bin';binary.mkdir()
        self.sources=names.names('test','macos-26-arm64','standard',4,8192,2)
        vms={name:dict(Name=name,OS='darwin',CPU=4,Memory=8192,State='stopped' if i==0 else 'suspended',MACAddress=f'old-{i}',MachineID='inherited',Release='old',IP=f'192.0.2.{i+1}') for i,name in enumerate(self.sources)}
        # An already-running job clone must remain unchanged through publication.
        vms['old-job']=dict(vms[self.sources[1]],Name='old-job',State='running',IP='192.0.2.4')
        self.orchard='orchard-'+self.sources[1]+'-12345678-1234-1234-1234-123456789012-0'
        vms[self.orchard]=dict(vms[self.sources[1]],Name=self.orchard,IP='192.0.2.5')
        self.before=json.loads(json.dumps(vms))
        self.state=self.root/'state.json';self.state.write_text(json.dumps(dict(vms=vms,calls=[],next=10)))
        for tool in ('tart','ssh','scp'):
            path=binary/tool;path.write_text('#!'+sys.executable+'\n'+FAKE);path.chmod(0o755)
        uname=binary/'uname';uname.write_text('#!/bin/bash\nif [[ $1 == -s ]]; then echo Darwin; else echo arm64; fi\n');uname.chmod(0o755)
        self.env=dict(os.environ,PATH=str(binary)+os.pathsep+os.environ['PATH'],FAKE_TART_STATE=str(self.state),FAKE_VM_FILES='1',TART_HOME=str(self.root/'tart'),PULSE_IMAGE_LOG_DIR=str(self.root/'logs'),PULSE_LEGACY_PROVIDERS_DIR=str(self.root/'legacy'))

    def run_update(self,*args):
        return subprocess.run(['bash',str(ROOT/'scripts/update-macos-images.sh'),'--owner','test','--slots','2','--c2j','a'*40,*args],env=self.env,capture_output=True,text=True,timeout=60)

    def test_publication_replaces_sources_and_keeps_running_clones(self):
        result=self.run_update();self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        state=json.loads(self.state.read_text())
        self.assertEqual(state['vms']['old-job'],self.before['old-job'])
        for name in [*self.sources,self.orchard]: self.assertEqual(state['vms'][name]['Release'],'new')
        self.assertEqual(state['vms'][self.sources[1]]['MACAddress'],state['vms'][self.orchard]['MACAddress'])
        self.assertNotEqual(state['vms'][self.sources[1]]['MACAddress'],state['vms'][self.sources[2]]['MACAddress'])
        machine_ids = [state['vms'][name].get('MachineID') for name in self.sources]
        self.assertEqual(len(set(machine_ids)), len(self.sources), 'Base and slots must have independent machine identities')
        self.assertNotIn(None, machine_ids)
        self.assertNotIn('inherited', machine_ids)
        self.assertEqual(state['vms'][self.sources[1]]['MachineID'], state['vms'][self.orchard]['MachineID'], 'Publication must preserve the saved machine identity')
        self.assertFalse(any(n.startswith('pulse-build-') for n in state['vms']))
        publishes=[i for i,c in enumerate(state['calls']) if c[:3]==['tart','clone','--overwrite']]
        suspends=[i for i,c in enumerate(state['calls']) if c[:2]==['tart','suspend']]
        self.assertGreater(min(publishes),max(suspends))

    def test_failed_preparation_does_not_publish_partial_sources(self):
        self.env['FAIL_SLOT']='2'
        result=self.run_update();self.assertNotEqual(result.returncode,0)
        state=json.loads(self.state.read_text())
        self.assertEqual(state['vms'],self.before)
        self.assertFalse(any(c[:3]==['tart','clone','--overwrite'] for c in state['calls']))

    def test_ensure_does_not_build_or_update_existing_sources(self):
        result=self.run_update('--ensure');self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        state=json.loads(self.state.read_text())
        self.assertEqual(state['vms'],self.before)
        self.assertFalse(any(c[0]=='ssh' or (c[:2]==['tart','clone'] and '--help' not in c) for c in state['calls']))

    def test_failed_publication_retains_prepared_sources_and_observes_changed_destination(self):
        self.env.update(FAIL_PUBLISH=self.sources[0], REMOVE_FAILED_DESTINATION='1')
        result=self.run_update()
        self.assertNotEqual(result.returncode,0)
        state=json.loads(self.state.read_text())
        builders={name:vm for name,vm in state['vms'].items() if name.startswith('pulse-build-')}
        self.assertEqual(len(builders),3)
        self.assertEqual(sorted(vm['State'] for vm in builders.values()),['stopped','suspended','suspended'])
        self.assertNotIn(self.sources[0],state['vms'])
        for name in [*self.sources[1:],'old-job',self.orchard]:
            self.assertEqual(state['vms'][name],self.before[name])
        self.assertIn('confirmed publications: 0',result.stderr)
        self.assertNotIn('Preparation failed;',result.stderr)
        self.assertTrue(all(name in result.stderr for name in builders))
        work=Path(next(line.removeprefix('Preparation logs: ') for line in result.stdout.splitlines() if line.startswith('Preparation logs: ')))
        self.addCleanup(shutil.rmtree,work)
        plan=json.loads((work/'prepared-sources.json').read_text())
        self.assertEqual({entry['prepared'] for entry in plan},set(builders))
        self.assertEqual(json.loads((work/'publication-0-target-before.json').read_text())['Name'],self.sources[0])
        after=json.loads((work/'publication-inventory.json').read_text())
        self.assertNotIn(self.sources[0],{vm['Name'] for vm in after})
        self.assertEqual((work/'publication-0.exit').read_text().strip(),'1')
        self.assertFalse(any(c[:2]==['tart','delete'] for c in state['calls']))

    def test_partial_publication_does_not_rollback_or_delete_running_jobs(self):
        self.env['FAIL_PUBLISH']=self.sources[1]
        result=self.run_update()
        self.assertNotEqual(result.returncode,0)
        state=json.loads(self.state.read_text())
        self.assertEqual(state['vms'][self.sources[0]]['Release'],'new')
        self.assertEqual(state['vms'][self.sources[1]],self.before[self.sources[1]])
        self.assertEqual(state['vms']['old-job'],self.before['old-job'])
        self.assertIn('confirmed publications: 1',result.stderr)
        work=Path(next(line.removeprefix('Preparation logs: ') for line in result.stdout.splitlines() if line.startswith('Preparation logs: ')))
        self.addCleanup(shutil.rmtree,work)

    def test_native_errors_after_complete_replacement_are_verified_and_update_continues(self):
        self.env.update(FAKE_VM_FILES='1', FAIL_AFTER_PUBLISH='*')
        result=self.run_update()
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        state=json.loads(self.state.read_text())
        for name in [*self.sources,self.orchard]: self.assertEqual(state['vms'][name]['Release'],'new')
        self.assertEqual(state['vms']['old-job'],self.before['old-job'])
        self.assertFalse(any(name.startswith('pulse-build-') for name in state['vms']))
        self.assertIn('Verified complete publication despite Tart exit 1',result.stderr)
        work=Path(next(line.removeprefix('Preparation logs: ') for line in result.stdout.splitlines() if line.startswith('Preparation logs: ')))
        self.addCleanup(shutil.rmtree,work)
        reports=list(work.glob('publication-*-verification.json'))
        self.assertEqual(len(reports),4)
        for report in reports: self.assertEqual(json.loads(report.read_text())['outcome'],'verified')

    def test_matching_hardware_with_corrupt_disk_never_counts_as_publication_success(self):
        self.env.update(FAKE_VM_FILES='1', FAIL_AFTER_PUBLISH=self.sources[0], CORRUPT_AFTER_PUBLISH='1')
        result=self.run_update()
        self.assertNotEqual(result.returncode,0)
        state=json.loads(self.state.read_text())
        self.assertEqual(len([name for name in state['vms'] if name.startswith('pulse-build-')]),3)
        self.assertEqual(state['vms']['old-job'],self.before['old-job'])
        self.assertIn('Publication could not be verified',result.stderr)
        work=Path(next(line.removeprefix('Preparation logs: ') for line in result.stdout.splitlines() if line.startswith('Preparation logs: ')))
        self.addCleanup(shutil.rmtree,work)
        self.assertEqual(json.loads((work/'publication-0-verification.json').read_text())['outcome'],'unverified')

    def test_ensure_builds_missing_second_slot_using_existing_base(self):
        state=json.loads(self.state.read_text())
        del state['vms'][self.sources[2]]
        self.state.write_text(json.dumps(state))
        before=state['vms']
        result=self.run_update('--ensure');self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        state=json.loads(self.state.read_text())
        for name,vm in before.items(): self.assertEqual(state['vms'][name],vm)
        new=state['vms'][self.sources[2]]
        self.assertEqual(new['State'],'suspended')
        self.assertNotEqual(new['MachineID'],state['vms'][self.sources[1]]['MachineID'])
        self.assertFalse(any(c[0]=='ssh' and 'prepare-base.sh' in c[-1] for c in state['calls']))
        self.assertEqual([c[-1] for c in state['calls'] if c[:3]==['tart','clone','--overwrite']],[self.sources[2]])

    def test_alias_matching_does_not_select_jobs_or_other_pools(self):
        inventory=[{'Name':self.orchard},{'Name':'old-job'},{'Name':self.orchard+'-extra'},{'Name':self.orchard.replace('pulse-test-','pulse-other-')}]
        self.assertEqual(names.aliases(self.sources[1],inventory),[self.sources[1],self.orchard])

    def retained_update(self):
        self.env['FAIL_PUBLISH']=self.sources[1]
        result=self.run_update()
        self.assertNotEqual(result.returncode,0,result.stdout+result.stderr)
        work=Path(next(line.removeprefix('Preparation logs: ') for line in result.stdout.splitlines() if line.startswith('Preparation logs: ')))
        self.addCleanup(shutil.rmtree,work)
        del self.env['FAIL_PUBLISH']
        return work

    def resume(self, work):
        return subprocess.run(['bash',str(ROOT/'scripts/update-macos-images.sh'),'--resume',str(work)],
                              env=self.env,capture_output=True,text=True,timeout=60)

    def test_resume_partial_publication_without_repreparing_or_touching_jobs(self):
        work=self.retained_update()
        before=json.loads(self.state.read_text());calls=len(before['calls'])
        result=self.resume(work)
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        after=json.loads(self.state.read_text())
        self.assertEqual(after['vms']['old-job'],self.before['old-job'])
        self.assertFalse(any(call[0]=='ssh' or call[:2]==['tart','run'] for call in after['calls'][calls:]))
        self.assertFalse(any(name.startswith('pulse-build-') for name in after['vms']))
        self.assertTrue(json.loads((work/'publication-plan.json').read_text())['complete'])
        for name in [*self.sources,self.orchard]: self.assertEqual(after['vms'][name]['Release'],'new')
        self.assertEqual(self.resume(work).returncode,0)

    def test_resume_refuses_changed_builder_before_any_publication(self):
        work=self.retained_update()
        plan=json.loads((work/'publication-plan.json').read_text())
        source=plan['items'][-1]['source']
        (Path(self.env['TART_HOME'])/'vms'/source/'disk.img').write_text('changed')
        before=json.loads(self.state.read_text())['calls']
        result=self.resume(work)
        self.assertNotEqual(result.returncode,0)
        self.assertIn('Prepared source changed',result.stderr)
        self.assertEqual(json.loads(self.state.read_text())['calls'],before)

    def test_resume_refuses_job_target_and_preserves_other_updater_lock(self):
        work=self.retained_update()
        lock=Path(self.env['TART_HOME'])/('.'+self.sources[0]+'.update-lock')
        lock.mkdir()
        result=self.resume(work)
        self.assertNotEqual(result.returncode,0)
        self.assertTrue(lock.is_dir())
        lock.rmdir()
        path=Path(self.env['TART_HOME'])/'vms'/self.sources[1]/'pulse.json'
        path.write_text(json.dumps({'native':{'reusable':True},'properties':{'job':{'id':'private'}}}))
        result=self.resume(work)
        self.assertNotEqual(result.returncode,0)
        self.assertIn('Refusing publication over a job',result.stderr)
        self.assertNotIn('private',result.stderr)


class DockerImageNames(unittest.TestCase):
    def test_docker_images_are_separate_and_version_independent(self):
        actual=names.names('mac-pool','macos-26-arm64','standard',4,8192,2,'engine')
        self.assertEqual(actual[0],'pulse-mac-pool-macos-26-arm64-docker-base')
        self.assertEqual(actual[1],'pulse-mac-pool-macos-26-arm64-standard-docker-engine-4cpu-8192m-slot-0001')
        self.assertEqual(len(set(actual)),3)

if __name__=='__main__': unittest.main()
