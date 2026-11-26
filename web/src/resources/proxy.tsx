// Administration resources for the second-development modules:
// RADIUS proxy upstreams, realm routes, Portal NAS, billing plans/accounts and
// CoA tasks. Kept in one file with a shared factory to stay maintainable.
import {
  List,
  Datagrid,
  TextField,
  NumberField,
  BooleanField,
  DateField,
  Edit,
  SimpleForm,
  TextInput,
  NumberInput,
  SelectInput,
  Create,
  Show,
  SimpleShowLayout,
  TopToolbar,
  ListButton,
  FunctionField,
} from 'react-admin';
import {
  CloudQueue as ProxyIcon,
  Route as RouteIcon,
  Wifi as PortalIcon,
  AccountBalanceWallet as BillingIcon,
  Gavel as CoAIcon,
} from '@mui/icons-material';
import type { ReactElement } from 'react';
import { cloneElement } from 'react';

type FieldKind = 'text' | 'number' | 'select' | 'date' | 'boolean';

interface FieldSpec {
  source: string;
  label?: string;
  kind?: FieldKind;
  choices?: Array<{ id: string | number; name: string }>;
}

interface ResourceSpec {
  name: string;
  label: string;
  fields: FieldSpec[];
  editable?: FieldSpec[];
  creatable?: FieldSpec[];
}

// Shared renderers -----------------------------------------------------------

const renderField = (spec: FieldSpec, edit: boolean): ReactElement => {
  const kind = spec.kind ?? 'text';
  if (edit) {
    switch (kind) {
      case 'number':
        return <NumberInput source={spec.source} label={spec.label} />;
      case 'select':
        return (
          <SelectInput
            source={spec.source}
            label={spec.label}
            choices={spec.choices ?? []}
          />
        );
      case 'boolean':
        return (
          <SelectInput
            source={spec.source}
            label={spec.label}
            choices={[
              { id: 'enabled', name: '启用' },
              { id: 'disabled', name: '禁用' },
            ]}
          />
        );
      default:
        return <TextInput source={spec.source} label={spec.label} />;
    }
  }
  switch (kind) {
    case 'number':
      return <NumberField source={spec.source} label={spec.label} />;
    case 'date':
      return <DateField source={spec.source} label={spec.label} showTime />;
    case 'boolean':
      return <BooleanField source={spec.source} label={spec.label} />;
    default:
      return <TextField source={spec.source} label={spec.label} />;
  }
};

const makeResource = (spec: ResourceSpec) => {
  const List_ = () => (
    <List>
      <Datagrid rowClick="show">
        {spec.fields.map((f) => cloneElement(renderField(f, false), { key: f.source }))}
      </Datagrid>
    </List>
  );

  const Show_ = () => (
    <Show actions={<TopToolbar><ListButton /></TopToolbar>}>
      <SimpleShowLayout>
        {spec.fields.map((f) => cloneElement(renderField(f, false), { key: f.source }))}
      </SimpleShowLayout>
    </Show>
  );

  const formFields = spec.editable ?? spec.creatable ?? [];
  const Edit_ = formFields.length
    ? () => (
        <Edit>
          <SimpleForm>
            {formFields.map((f) => cloneElement(renderField(f, true), { key: f.source }))}
          </SimpleForm>
        </Edit>
      )
    : undefined;

  const Create_ = spec.creatable?.length
    ? () => (
        <Create>
          <SimpleForm>
            {spec.creatable!.map((f) => cloneElement(renderField(f, true), { key: f.source }))}
          </SimpleForm>
        </Create>
      )
    : undefined;

  return { List: List_, Show: Show_, Edit: Edit_, Create: Create_ };
};

const statusChoices = [
  { id: 'enabled', name: '启用' },
  { id: 'disabled', name: '禁用' },
];

// 1. RADIUS upstreams -------------------------------------------------------
const upstreamFields: FieldSpec[] = [
  { source: 'id', label: 'ID' },
  { source: 'name', label: '名称' },
  { source: 'addr', label: '地址' },
  { source: 'auth_port', label: '认证端口', kind: 'number' },
  { source: 'acct_port', label: '计费端口', kind: 'number' },
  { source: 'coa_port', label: 'CoA端口', kind: 'number' },
  { source: 'role', label: '角色', kind: 'select', choices: [{ id: 'proxy', name: '代理' }, { id: 'replicate', name: '抄送' }] },
  { source: 'weight', label: '权重', kind: 'number' },
  { source: 'status', label: '状态', kind: 'select', choices: statusChoices },
];
const upstreamForm: FieldSpec[] = [
  { source: 'name', label: '名称', kind: 'text' },
  { source: 'addr', label: '地址', kind: 'text' },
  { source: 'auth_port', label: '认证端口', kind: 'number' },
  { source: 'acct_port', label: '计费端口', kind: 'number' },
  { source: 'coa_port', label: 'CoA端口', kind: 'number' },
  { source: 'secret', label: '共享密钥', kind: 'text' },
  { source: 'role', label: '角色', kind: 'select', choices: [{ id: 'proxy', name: '代理' }, { id: 'replicate', name: '抄送' }] },
  { source: 'weight', label: '权重', kind: 'number' },
  { source: 'status', label: '状态', kind: 'select', choices: statusChoices },
  { source: 'remark', label: '备注', kind: 'text' },
];
const upstream = makeResource({
  name: 'radius-upstreams',
  label: 'RADIUS 上游',
  fields: upstreamFields,
  editable: upstreamForm,
  creatable: upstreamForm,
});
export const ProxyUpstreamList = upstream.List;
export const ProxyUpstreamEdit = upstream.Edit;
export const ProxyUpstreamCreate = upstream.Create;
export const ProxyUpstreamShow = upstream.Show;
export const ProxyUpstreamIcon = ProxyIcon;

// 2. Realm routes -----------------------------------------------------------
const routeFields: FieldSpec[] = [
  { source: 'id', label: 'ID' },
  { source: 'realm', label: '域' },
  { source: 'strategy', label: '策略', kind: 'select', choices: [{ id: 'single', name: '单发' }, { id: 'round_robin', name: '轮询' }, { id: 'weight', name: '加权' }] },
  { source: 'upstream_ids', label: '上游ID' },
  { source: 'user_prefix', label: '用户名前缀' },
  { source: 'user_suffix', label: '用户名后缀' },
  { source: 'mapped_password', label: '映射密码' },
  { source: 'acct_mode', label: '计费模式', kind: 'select', choices: [{ id: 'both', name: '双写' }, { id: 'forward', name: '转发' }, { id: 'local', name: '本地' }] },
  { source: 'status', label: '状态', kind: 'select', choices: statusChoices },
];
const routeForm: FieldSpec[] = [
  { source: 'realm', label: '域（* 为兜底）', kind: 'text' },
  { source: 'strategy', label: '策略', kind: 'select', choices: [{ id: 'single', name: '单发' }, { id: 'round_robin', name: '轮询' }, { id: 'weight', name: '加权' }] },
  { source: 'upstream_ids', label: '上游ID（逗号分隔）', kind: 'text' },
  { source: 'user_prefix', label: '用户名前缀', kind: 'text' },
  { source: 'user_suffix', label: '用户名后缀', kind: 'text' },
  { source: 'mapped_password', label: '映射密码', kind: 'text' },
  { source: 'acct_mode', label: '计费模式', kind: 'select', choices: [{ id: 'both', name: '双写' }, { id: 'forward', name: '转发' }, { id: 'local', name: '本地' }] },
  { source: 'status', label: '状态', kind: 'select', choices: statusChoices },
];
const realmRoute = makeResource({
  name: 'radius-realm-routes',
  label: '域路由',
  fields: routeFields,
  editable: routeForm,
  creatable: routeForm,
});
export const RealmRouteList = realmRoute.List;
export const RealmRouteEdit = realmRoute.Edit;
export const RealmRouteCreate = realmRoute.Create;
export const RealmRouteShow = realmRoute.Show;
export const RealmRouteIcon = RouteIcon;

// 3. Portal NAS -------------------------------------------------------------
const portalNasFields: FieldSpec[] = [
  { source: 'id', label: 'ID' },
  { source: 'name', label: '名称' },
  { source: 'addr', label: '地址' },
  { source: 'secret', label: '密钥' },
  { source: 'status', label: '状态', kind: 'select', choices: statusChoices },
  { source: 'remark', label: '备注' },
];
const portalNasForm: FieldSpec[] = [
  { source: 'name', label: '名称', kind: 'text' },
  { source: 'addr', label: '地址', kind: 'text' },
  { source: 'secret', label: '密钥', kind: 'text' },
  { source: 'status', label: '状态', kind: 'select', choices: statusChoices },
  { source: 'remark', label: '备注', kind: 'text' },
];
const portalNas = makeResource({
  name: 'portal-nases',
  label: 'Portal设备',
  fields: portalNasFields,
  editable: portalNasForm,
  creatable: portalNasForm,
});
export const PortalNasList = portalNas.List;
export const PortalNasEdit = portalNas.Edit;
export const PortalNasCreate = portalNas.Create;
export const PortalNasShow = portalNas.Show;
export const PortalNasIcon = PortalIcon;

// 4. Billing plans / accounts (read-mostly) ---------------------------------
const accountFields: FieldSpec[] = [
  { source: 'id', label: 'ID' },
  { source: 'username', label: '用户名' },
  { source: 'balance_fen', label: '余额(分)', kind: 'number' },
  { source: 'time_seconds', label: '时长(秒)', kind: 'number' },
  { source: 'bytes_quota', label: '流量配额', kind: 'number' },
  { source: 'status', label: '状态' },
  { source: 'updated_at', label: '更新时间', kind: 'date' },
];
const billingAccount = makeResource({ name: 'billing/accounts', label: '计费账户', fields: accountFields });
export const BillingAccountList = billingAccount.List;
export const BillingAccountShow = billingAccount.Show;
export const BillingIconExport = BillingIcon;

const orderFields: FieldSpec[] = [
  { source: 'order_no', label: '订单号' },
  { source: 'account_id', label: '账户ID', kind: 'number' },
  { source: 'plan_id', label: '套餐ID', kind: 'number' },
  { source: 'amount_fen', label: '金额(分)', kind: 'number' },
  { source: 'channel', label: '渠道' },
  { source: 'status', label: '状态' },
  { source: 'created_at', label: '创建时间', kind: 'date' },
];
const billingOrder = makeResource({ name: 'billing/orders', label: '订单', fields: orderFields });
export const BillingOrderList = billingOrder.List;
export const BillingOrderShow = billingOrder.Show;

// 5. CoA tasks (read-only audit) --------------------------------------------
const coaTaskFields: FieldSpec[] = [
  { source: 'id', label: 'ID' },
  { source: 'session_id', label: '会话ID', kind: 'number' },
  { source: 'username', label: '用户名' },
  { source: 'action', label: '动作' },
  { source: 'status', label: '状态' },
  { source: 'identifier', label: 'Identifier', kind: 'number' },
  { source: 'error_cause', label: 'Error-Cause', kind: 'number' },
  { source: 'retry_count', label: '重试次数', kind: 'number' },
  { source: 'created_at', label: '创建时间', kind: 'date' },
];
const coaTask = makeResource({ name: 'coa-tasks', label: 'CoA任务', fields: coaTaskFields });
export const CoATaskList = coaTask.List;
export const CoATaskShow = coaTask.Show;
export const CoATaskIcon = CoAIcon;

