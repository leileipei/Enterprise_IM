"""Strict contracts for the limited independent native messaging scenario."""
import re
from urllib.parse import urlparse

ROLES={'alice':'android','bob_ios':'ios','bob_android':'android'}
# messages; alice d/r; bob-ios d/r; bob-android d/r; user alice/bob reads.
MATRIX={'registered':(0,0,0,0,0,0,0,0,0),'first_sent':(1,0,0,0,0,0,0,0,0),'bob_delivered':(1,0,0,1,0,0,0,0,0),'both_delivered':(1,0,0,1,0,1,0,0,0),'bob_read':(1,0,0,1,1,1,0,0,1),'offline_sent':(2,0,0,2,2,1,0,0,2),'offline_stable':(2,0,0,2,2,1,0,0,2),'recovered_delivered':(2,0,0,2,2,2,0,0,2),'recovered_read':(2,0,0,2,2,2,2,0,2),'reply_read':(3,1,1,2,2,2,2,1,2),'done':(3,1,1,2,2,2,2,1,2)}


def actor_config(config,role,nonce):
    if role not in ROLES or not re.fullmatch('[a-f0-9]{32}',nonce):raise ValueError('cross_actor_invalid')
    origin=urlparse(config['origin'])
    if origin.scheme!='https' or origin.hostname!='127.0.0.1' or not origin.port or origin.username or origin.password or origin.path not in ('','/') or origin.query or origin.fragment:raise ValueError('cross_origin_not_owned_loopback')
    actor=config['actors'][role]
    if actor['platform']!=ROLES[role]:raise ValueError('cross_actor_platform_mismatch')
    return {**{k:config[k] for k in ('origin','ca_pem','tenant','conversation')},**{k:actor[k] for k in ('platform','user','membership','refresh')},'role':role,'nonce':nonce}


def network_rule(uid,comment,action):
    if type(uid) is not int or not 10000<=uid<200000 or not isinstance(comment,str) or not re.fullmatch(r'IMCROSS_[A-Z0-9]{1,16}',comment) or action not in ('insert','delete'):raise ValueError('cross_network_rule_invalid')
    return ['shell','su','0','iptables',*(['-I','OUTPUT','1'] if action=='insert' else ['-D','OUTPUT']),'-m','owner','--uid-owner',str(uid),'-m','comment','--comment',comment,'-j','REJECT']


def validate_participants(states):
    if set(states)!=set(ROLES):raise ValueError('cross_three_actors_required')
    for role,platform in ROLES.items():
        state=states[role]
        if state.get('role')!=role or state.get('platform')!=platform or type(state.get('pid')) is not int or state['pid']<=0 or not isinstance(state.get('nonce'),str) or not re.fullmatch('[a-f0-9]{32}',state['nonce']) or not isinstance(state.get('device_id'),str) or not state['device_id']:raise ValueError('cross_actor_identity_invalid')
    if len({s['device_id'] for s in states.values()})!=3 or len({s['nonce'] for s in states.values()})!=3 or states['bob_ios']['user']!=states['bob_android']['user'] or states['alice']['user']==states['bob_ios']['user']:raise ValueError('cross_distinct_devices_and_accounts_required')


def validate_ack(state,expected,command):
    if any(state.get(k)!=expected[k] for k in ('role','nonce','pid')) or type(state.get('id')) is not int or state['id']!=command or state.get('status') not in ('ok','done'):raise ValueError('cross_actor_ack_invalid')


def checkpoint(phase,facts):
    if phase not in MATRIX:raise ValueError('cross_phase_unknown')
    try:
        actual=[facts['messages']]
        for role in ROLES:actual.extend(facts['roles'][role][k] for k in ('delivered','read'))
        actual.extend(facts['users'][k] for k in ('alice','bob'))
        if type(facts['devices']) is not int or facts['devices']!=3 or any(type(x) is not int for x in actual) or tuple(actual)!=MATRIX[phase]:raise ValueError('cross_database_checkpoint_mismatch')
    except (KeyError,TypeError):raise ValueError('cross_database_checkpoint_malformed') from None
